package mail_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// testServer scripts one SMTP connection to exercise the transport over the wire.
type testServer struct {
	ln net.Listener

	starttls bool   // advertises and performs STARTTLS
	authUser string // enables AUTH PLAIN with authPass
	authPass string
	stallAt  string // phase to stall: "banner", "ehlo", or "data"
	failQuit bool

	implicitTLS bool

	// accepted closes when QUIT proves the client read the DATA acceptance, while
	// stalled closes when stallAt is reached.
	accepted  chan struct{}
	stalled   chan struct{}
	quitDelay time.Duration

	mu       sync.Mutex
	commands []string
	data     string
	authOK   bool
}

func newTestServer(t *testing.T, mutate func(*testServer)) *testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{ln: ln, accepted: make(chan struct{}), stalled: make(chan struct{})}
	if mutate != nil {
		mutate(s)
	}
	t.Cleanup(func() { ln.Close() })
	go s.serve(t)
	return s
}

func (s *testServer) addr() string { return s.ln.Addr().String() }

// stall signals its phase and waits for the peer to close the connection.
func (s *testServer) stall(conn net.Conn) {
	close(s.stalled)
	buf := make([]byte, 256)
	for {
		if _, err := conn.Read(buf); err != nil {
			return
		}
	}
}

func (s *testServer) session() (commands []string, data string, authOK bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...), s.data, s.authOK
}

func (s *testServer) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, strings.TrimRight(line, "\r\n"))
}

func (s *testServer) serve(t *testing.T) {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	if s.implicitTLS {
		tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{testCert}, MinVersion: tls.VersionTLS12})
		conn = tc
	}
	if s.stallAt == "banner" {
		s.stall(conn)
		return
	}
	fmt.Fprintf(conn, "220 fake ESMTP\r\n")
	r := bufio.NewReader(conn)
	secured := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		s.record(line)
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			if s.stallAt == "ehlo" {
				s.stall(conn)
				return
			}
			// net/smtp discards the first EHLO line as the server domain.
			lines := []string{"fake"}
			if s.starttls && !secured {
				lines = append(lines, "STARTTLS")
			}
			if s.authUser != "" && (secured || !s.starttls) {
				lines = append(lines, "AUTH PLAIN")
			}
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				fmt.Fprintf(conn, "250%s%s\r\n", sep, l)
			}
		case cmd == "STARTTLS":
			fmt.Fprintf(conn, "220 go ahead\r\n")
			tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{testCert}, MinVersion: tls.VersionTLS12})
			conn = tc
			r = bufio.NewReader(conn)
			secured = true
		case strings.HasPrefix(cmd, "AUTH PLAIN"):
			payload := strings.TrimPrefix(strings.TrimSpace(line), "AUTH PLAIN ")
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
			parts := strings.Split(string(raw), "\x00")
			if len(parts) == 3 && parts[1] == s.authUser && parts[2] == s.authPass {
				s.mu.Lock()
				s.authOK = true
				s.mu.Unlock()
				fmt.Fprintf(conn, "235 ok\r\n")
			} else {
				fmt.Fprintf(conn, "535 bad credentials\r\n")
			}
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
			fmt.Fprintf(conn, "250 ok\r\n")
		case cmd == "DATA":
			if s.stallAt == "data" {
				s.stall(conn)
				return
			}
			fmt.Fprintf(conn, "354 go\r\n")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				data.WriteString(l)
			}
			s.mu.Lock()
			s.data = data.String()
			s.mu.Unlock()
			fmt.Fprintf(conn, "250 accepted\r\n")
		case cmd == "QUIT":
			close(s.accepted)
			if s.quitDelay > 0 {
				time.Sleep(s.quitDelay)
			}
			if s.failQuit {
				fmt.Fprintf(conn, "421 shutting down\r\n")
			} else {
				fmt.Fprintf(conn, "221 bye\r\n")
			}
			return
		default:
			fmt.Fprintf(conn, "250 ok\r\n")
		}
	}
}

// testCertPool trusts the self-signed testCert for 127.0.0.1.
var testCert, testCertPool = func() (tls.Certificate, *x509.CertPool) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}()
