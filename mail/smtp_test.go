package mail_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/mail"
	"github.com/gofabrik/fabrik/mail/transporttest"
)

func smtpMessage() mail.Message {
	return mail.Message{
		From:    `"Fabrik" <noreply@fabrik.test>`,
		To:      []string{"ada@example.com"},
		Cc:      []string{"ops@example.com"},
		Bcc:     []string{"archive@example.com"},
		Subject: "Hello",
		Text:    "Hi!",
	}
}

func trusted() *tls.Config {
	return &tls.Config{RootCAs: testCertPool, MinVersion: tls.VersionTLS12}
}

func TestSMTP_PlaintextSession(t *testing.T) {
	srv := newTestServer(t, nil)
	tr := &mail.SMTP{Addr: srv.addr(), TLSMode: mail.TLSModePlaintext}
	m := smtpMessage()
	if err := tr.Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	commands, data, _ := srv.session()
	joined := strings.Join(commands, "\n")
	for _, needle := range []string{
		"MAIL FROM:<noreply@fabrik.test>",
		"RCPT TO:<ada@example.com>",
		"RCPT TO:<ops@example.com>",
		"RCPT TO:<archive@example.com>",
		"QUIT",
	} {
		if !strings.Contains(joined, needle) {
			t.Errorf("session missing %q:\n%s", needle, joined)
		}
	}
	if !strings.Contains(data, "Subject: Hello") || strings.Contains(data, "Bcc:") {
		t.Errorf("DATA payload wrong (Bcc must stay envelope-only):\n%s", data)
	}
}

func TestSMTP_STARTTLSDefault(t *testing.T) {
	srv := newTestServer(t, func(s *testServer) { s.starttls = true })
	tr := &mail.SMTP{Addr: srv.addr(), TLSConfig: trusted()}
	m := smtpMessage()
	if err := tr.Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	commands, data, _ := srv.session()
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "STARTTLS") {
		t.Fatalf("session never upgraded:\n%s", joined)
	}
	if !strings.Contains(data, "Subject: Hello") {
		t.Errorf("post-TLS DATA payload missing:\n%s", data)
	}
}

func TestSMTP_STARTTLSRefusedWhenNotOffered(t *testing.T) {
	srv := newTestServer(t, nil)
	tr := &mail.SMTP{Addr: srv.addr()}
	m := smtpMessage()
	err := tr.Send(context.Background(), &m)
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("err = %v, want a STARTTLS refusal naming the plaintext opt-out", err)
	}
}

func TestSMTP_ImplicitTLSSession(t *testing.T) {
	srv := newTestServer(t, func(s *testServer) { s.implicitTLS = true })
	tr := &mail.SMTP{Addr: srv.addr(), TLSMode: mail.TLSModeImplicit, TLSConfig: trusted()}
	m := smtpMessage()
	if err := tr.Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if _, data, _ := srv.session(); !strings.Contains(data, "Subject: Hello") {
		t.Errorf("implicit-TLS DATA payload missing:\n%s", data)
	}
}

func TestSMTP_ExplicitSTARTTLSSpelling(t *testing.T) {
	srv := newTestServer(t, func(s *testServer) { s.starttls = true })
	tr := &mail.SMTP{Addr: srv.addr(), TLSMode: "starttls", TLSConfig: trusted()}
	m := smtpMessage()
	if err := tr.Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if commands, _, _ := srv.session(); !strings.Contains(strings.Join(commands, "\n"), "STARTTLS") {
		t.Fatal("explicit starttls spelling must negotiate STARTTLS")
	}
}

func TestSMTP_AuthExchange(t *testing.T) {
	srv := newTestServer(t, func(s *testServer) {
		s.starttls = true
		s.authUser, s.authPass = "mailer", "s3cret"
	})
	tr := &mail.SMTP{Addr: srv.addr(), TLSConfig: trusted(), Username: "mailer", Password: "s3cret"}
	m := smtpMessage()
	if err := tr.Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if _, _, authOK := srv.session(); !authOK {
		t.Fatal("server never saw valid credentials")
	}
}

func TestSMTP_AuthRejected(t *testing.T) {
	srv := newTestServer(t, func(s *testServer) {
		s.starttls = true
		s.authUser, s.authPass = "mailer", "s3cret"
	})
	tr := &mail.SMTP{Addr: srv.addr(), TLSConfig: trusted(), Username: "mailer", Password: "wrong"}
	m := smtpMessage()
	if err := tr.Send(context.Background(), &m); err == nil {
		t.Fatal("bad credentials accepted")
	}
}

func TestSMTP_CancellationInterruptsStalls(t *testing.T) {
	for _, phase := range []string{"banner", "ehlo", "data"} {
		t.Run(phase, func(t *testing.T) {
			srv := newTestServer(t, func(s *testServer) { s.stallAt = phase })
			tr := &mail.SMTP{Addr: srv.addr(), TLSMode: mail.TLSModePlaintext}
			ctx, cancel := context.WithCancel(context.Background())
			// Wait until the server reaches the stalled phase before canceling.
			go func() { <-srv.stalled; cancel() }()
			m := smtpMessage()
			start := time.Now()
			err := tr.Send(ctx, &m)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("cancellation took %v; the stalled session must be interrupted", elapsed)
			}
		})
	}
}

func TestSMTP_CommitPoint(t *testing.T) {
	t.Run("quit failure still succeeds", func(t *testing.T) {
		srv := newTestServer(t, func(s *testServer) { s.failQuit = true })
		tr := &mail.SMTP{Addr: srv.addr(), TLSMode: mail.TLSModePlaintext}
		m := smtpMessage()
		if err := tr.Send(context.Background(), &m); err != nil {
			t.Fatalf("Send = %v; a QUIT failure after DATA acceptance must not fail the send", err)
		}
	})
	t.Run("post-acceptance cancel still succeeds", func(t *testing.T) {
		srv := newTestServer(t, func(s *testServer) { s.quitDelay = 50 * time.Millisecond })
		tr := &mail.SMTP{Addr: srv.addr(), TLSMode: mail.TLSModePlaintext}
		ctx, cancel := context.WithCancel(context.Background())
		go func() { <-srv.accepted; cancel() }()
		m := smtpMessage()
		if err := tr.Send(ctx, &m); err != nil {
			t.Fatalf("Send = %v; cancellation after acceptance must not fail the send", err)
		}
	})
}

func TestSMTP_TLSConfigClonedNotMutated(t *testing.T) {
	srv := newTestServer(t, func(s *testServer) { s.starttls = true })
	cfg := trusted() // deliberately no ServerName; the clone must fill it
	tr := &mail.SMTP{Addr: srv.addr(), TLSConfig: cfg}
	m := smtpMessage()
	if err := tr.Send(context.Background(), &m); err != nil {
		t.Fatalf("verification through the cloned config failed: %v", err)
	}
	if cfg.ServerName != "" {
		t.Fatalf("caller's TLSConfig was mutated: ServerName = %q", cfg.ServerName)
	}
}

func TestSMTP_ValidateMatrix(t *testing.T) {
	cases := map[string]mail.SMTP{
		"missing addr":  {},
		"bad addr":      {Addr: "no-port"},
		"unknown mode":  {Addr: "h:25", TLSMode: "startls"},
		"password only": {Addr: "h:25", Password: "x"},
	}
	for name, tr := range cases {
		if err := tr.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want error", name)
		}
	}
	for _, mode := range []mail.TLSMode{mail.TLSModeSTARTTLS, "starttls", mail.TLSModeImplicit, mail.TLSModePlaintext} {
		ok := mail.SMTP{Addr: "h:25", TLSMode: mode}
		if err := ok.Validate(); err != nil {
			t.Errorf("mode %q must validate: %v", mode, err)
		}
	}
	d := &countingDialer{}
	m := smtpMessage()
	bad := mail.SMTP{Addr: "h:25", TLSMode: "startls", Dialer: d}
	if err := bad.Send(context.Background(), &m); err == nil || !strings.Contains(err.Error(), "unknown TLS mode") {
		t.Errorf("Send must enforce Validate, got %v", err)
	}
	if n := d.dials.Load(); n != 0 {
		t.Errorf("an unknown TLS mode dialed %d times; it must never reach the network", n)
	}
}

type countingDialer struct {
	dials atomic.Int32
}

func (d *countingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d.dials.Add(1)
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

func TestSMTP_NoDialOnInvalidOrPreCanceled(t *testing.T) {
	d := &countingDialer{}
	tr := &mail.SMTP{Addr: "127.0.0.1:1", TLSMode: mail.TLSModePlaintext, Dialer: d}

	bad := smtpMessage()
	bad.From = ""
	if err := tr.Send(context.Background(), &bad); err == nil {
		t.Fatal("invalid message accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := smtpMessage()
	if err := tr.Send(ctx, &m); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := d.dials.Load(); n != 0 {
		t.Fatalf("rejected sends dialed %d times", n)
	}
}

func TestSMTP_ContextPrecedence(t *testing.T) {
	t.Run("pre-canceled beats invalid config", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		m := smtpMessage()
		tr := &mail.SMTP{Addr: "no-port", TLSMode: "bogus"}
		if err := tr.Send(ctx, &m); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled before any validation", err)
		}
	})
	t.Run("cancellation during dial beats the dial error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		sentinel := errors.New("sentinel dial failure")
		d := dialerFunc(func(dctx context.Context, network, addr string) (net.Conn, error) {
			cancel()
			return nil, sentinel
		})
		m := smtpMessage()
		tr := &mail.SMTP{Addr: "127.0.0.1:1", TLSMode: mail.TLSModePlaintext, Dialer: d}
		err := tr.Send(ctx, &m)
		if !errors.Is(err, context.Canceled) || errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want the context error, not the dialer's sentinel", err)
		}
	})
}

type dialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (f dialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

func TestSMTP_Conformance(t *testing.T) {
	transporttest.Run(t, func() mail.Transport {
		srv := newTestServer(t, func(s *testServer) { s.starttls = true })
		return &mail.SMTP{Addr: srv.addr(), TLSConfig: trusted()}
	})
}
