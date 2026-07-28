package mail

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	netmail "net/mail"
	"net/textproto"
	"strings"
	"time"
)

// wireMessage omits Bcc headers and folds headers at 78 columns.
func wireMessage(m *Message, now time.Time) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	var b strings.Builder

	from, err := encodeAddress(m.From)
	if err != nil {
		return "", err
	}
	foldHeader(&b, "From", from)
	if err := addressHeader(&b, "To", m.To); err != nil {
		return "", err
	}
	if err := addressHeader(&b, "Cc", m.Cc); err != nil {
		return "", err
	}
	if m.ReplyTo != "" {
		rt, err := encodeAddress(m.ReplyTo)
		if err != nil {
			return "", err
		}
		foldHeader(&b, "Reply-To", rt)
	}
	foldHeader(&b, "Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	foldHeader(&b, "Date", now.Format(time.RFC1123Z))
	if m.ID != "" {
		foldHeader(&b, "Message-ID", "<"+m.ID+">")
	}
	foldHeader(&b, "MIME-Version", "1.0")

	switch {
	case len(m.Attachments) > 0:
		if err := writeMixed(&b, m); err != nil {
			return "", err
		}
	case m.HTML != "":
		if err := writeAlternative(&b, m); err != nil {
			return "", err
		}
	default:
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		encoded, err := qp(m.Text)
		if err != nil {
			return "", err
		}
		b.WriteString(encoded)
	}
	return b.String(), nil
}

func addressHeader(b *strings.Builder, name string, addrs []string) error {
	if len(addrs) == 0 {
		return nil
	}
	parts := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		enc, err := encodeAddress(addr)
		if err != nil {
			return err
		}
		parts = append(parts, enc)
	}
	foldHeader(b, name, strings.Join(parts, ", "))
	return nil
}

// foldHeader wraps at 78 columns; a bare field name followed by a continuation
// is valid RFC 5322 folding when the first word does not fit.
func foldHeader(b *strings.Builder, name, value string) {
	line := name + ":"
	for _, word := range strings.Split(value, " ") {
		if len(line)+1+len(word) > 78 {
			b.WriteString(line)
			b.WriteString("\r\n")
			line = " " + word
			continue
		}
		line += " " + word
	}
	b.WriteString(line)
	b.WriteString("\r\n")
}

func writeAlternative(b *strings.Builder, m *Message) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := writeBodyParts(mw, m); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("mail: close alternative MIME body: %w", err)
	}
	b.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=%q\r\n\r\n", mw.Boundary()))
	b.Write(body.Bytes())
	return nil
}

func writeBodyParts(mw *multipart.Writer, m *Message) error {
	text, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	if err != nil {
		return fmt.Errorf("mail: create plain-text MIME part: %w", err)
	}
	encoded, err := qp(m.Text)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(text, encoded); err != nil {
		return fmt.Errorf("mail: write plain-text MIME part: %w", err)
	}
	if m.HTML != "" {
		html, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {"text/html; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return fmt.Errorf("mail: create HTML MIME part: %w", err)
		}
		encoded, err := qp(m.HTML)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprint(html, encoded); err != nil {
			return fmt.Errorf("mail: write HTML MIME part: %w", err)
		}
	}
	return nil
}

func writeMixed(b *strings.Builder, m *Message) error {
	var body bytes.Buffer
	outer := multipart.NewWriter(&body)

	if m.HTML != "" {
		var alt bytes.Buffer
		inner := multipart.NewWriter(&alt)
		if err := writeBodyParts(inner, m); err != nil {
			return err
		}
		if err := inner.Close(); err != nil {
			return fmt.Errorf("mail: close alternative MIME body: %w", err)
		}
		part, err := outer.CreatePart(textproto.MIMEHeader{
			"Content-Type": {mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": inner.Boundary()})},
		})
		if err != nil {
			return fmt.Errorf("mail: create alternative MIME part: %w", err)
		}
		if _, err := part.Write(alt.Bytes()); err != nil {
			return fmt.Errorf("mail: write alternative MIME part: %w", err)
		}
	} else {
		if err := writeBodyParts(outer, m); err != nil {
			return err
		}
	}

	for _, a := range m.Attachments {
		ct := a.ContentType
		if ct == "" {
			ct = http.DetectContentType(a.Content)
		}
		part, err := outer.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {ct},
			"Content-Transfer-Encoding": {"base64"},
			"Content-Disposition":       {mime.FormatMediaType("attachment", map[string]string{"filename": a.Filename})},
		})
		if err != nil {
			return err
		}
		if _, err := part.Write([]byte(wrapBase64(a.Content))); err != nil {
			return fmt.Errorf("mail: write attachment %q: %w", a.Filename, err)
		}
	}
	if err := outer.Close(); err != nil {
		return fmt.Errorf("mail: close mixed MIME body: %w", err)
	}
	b.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=%q\r\n\r\n", outer.Boundary()))
	b.Write(body.Bytes())
	return nil
}

func qp(s string) (string, error) {
	var b strings.Builder
	w := quotedprintable.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		return "", fmt.Errorf("mail: encode quoted-printable body: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("mail: finish quoted-printable body: %w", err)
	}
	return b.String(), nil
}

// wrapBase64 limits encoded lines to RFC 2045's 76 columns.
func wrapBase64(content []byte) string {
	enc := base64.StdEncoding.EncodeToString(content)
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76])
		b.WriteString("\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	b.WriteString("\r\n")
	return b.String()
}

func encodeAddress(addr string) (string, error) {
	a, err := netmail.ParseAddress(addr)
	if err != nil {
		return "", fmt.Errorf("mail: invalid address %q: %w", addr, err)
	}
	return a.String(), nil
}
