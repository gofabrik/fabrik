package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	netmail "net/mail"
	"net/smtp"
	"time"
)

// TLSMode selects how SMTP sessions are secured; its zero value requires STARTTLS.
type TLSMode string

const (
	// TLSModeSTARTTLS requires STARTTLS before authentication or delivery;
	// the string "starttls" selects the same mode.
	TLSModeSTARTTLS TLSMode = ""
	// TLSModeImplicit speaks TLS from the first byte (port 465).
	TLSModeImplicit TLSMode = "implicit"
	// TLSModePlaintext disables TLS entirely for local relays.
	TLSModePlaintext TLSMode = "plaintext"
)

// ContextDialer establishes a connection with context cancellation.
type ContextDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// SMTP delivers messages over SMTP; Addr is required and credentials are used
// only when Username is set.
type SMTP struct {
	Addr      string // host:port
	Username  string
	Password  string
	TLSMode   TLSMode
	TLSConfig *tls.Config // optional; cloned before ServerName is defaulted
	Dialer    ContextDialer
}

// Validate reports the first SMTP configuration error.
func (s *SMTP) Validate() error {
	if s.Addr == "" {
		return fmt.Errorf("mail: SMTP needs Addr")
	}
	if _, _, err := net.SplitHostPort(s.Addr); err != nil {
		return fmt.Errorf("mail: SMTP Addr: %w", err)
	}
	switch s.TLSMode {
	case TLSModeSTARTTLS, "starttls", TLSModeImplicit, TLSModePlaintext:
	default:
		return fmt.Errorf("mail: unknown TLS mode %q (use starttls, implicit, or plaintext)", s.TLSMode)
	}
	if s.Password != "" && s.Username == "" {
		return fmt.Errorf("mail: SMTP Password without Username")
	}
	return nil
}

// Send delivers m and closes the connection on context cancellation; delivery
// succeeds once the server accepts the message data, regardless of later cleanup
// errors or cancellation.
func (s *SMTP) Send(ctx context.Context, m *Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.Validate(); err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return err
	}
	wire, err := wireMessage(m, time.Now())
	if err != nil {
		return err
	}

	dialer := s.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	rawConn, err := dialer.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("mail: send to %s: %w", s.Addr, ctx.Err())
		}
		return fmt.Errorf("mail: dial %s: %w", s.Addr, err)
	}
	// Closing the connection makes context cancellation interrupt net/smtp I/O.
	stop := context.AfterFunc(ctx, func() { rawConn.Close() })
	defer stop()
	wrap := func(err error) error {
		if ctx.Err() != nil {
			return fmt.Errorf("mail: send to %s: %w", s.Addr, ctx.Err())
		}
		return fmt.Errorf("mail: send to %s: %w", s.Addr, err)
	}

	host, _, err := net.SplitHostPort(s.Addr)
	if err != nil {
		rawConn.Close()
		return wrap(err)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if s.TLSConfig != nil {
		tlsCfg = s.TLSConfig.Clone()
	}
	if tlsCfg.ServerName == "" {
		tlsCfg.ServerName = host
	}

	conn := rawConn
	if s.TLSMode == TLSModeImplicit {
		conn = tls.Client(rawConn, tlsCfg)
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return wrap(err)
	}
	defer c.Close()

	if s.TLSMode == TLSModeSTARTTLS || s.TLSMode == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return wrap(fmt.Errorf("server does not offer STARTTLS (set TLSMode to plaintext to opt out)"))
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return wrap(err)
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, host)); err != nil {
			return wrap(err)
		}
	}
	if err := c.Mail(envelopeAddress(m.From)); err != nil {
		return wrap(err)
	}
	for _, rcpt := range m.recipients() {
		if err := c.Rcpt(envelopeAddress(rcpt)); err != nil {
			return wrap(err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return wrap(err)
	}
	if _, err := w.Write([]byte(wire)); err != nil {
		return wrap(err)
	}
	if err := w.Close(); err != nil {
		return wrap(err)
	}
	// DATA acceptance commits delivery, so QUIT errors are ignored.
	c.Quit()
	return nil
}

func envelopeAddress(addr string) string {
	if a, err := netmail.ParseAddress(addr); err == nil {
		return a.Address
	}
	return addr
}
