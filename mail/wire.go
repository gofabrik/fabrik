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
		writeAlternative(&b, m)
	default:
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		b.WriteString(qp(m.Text))
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

func writeAlternative(b *strings.Builder, m *Message) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	writeBodyParts(mw, m)
	mw.Close()
	fmt.Fprintf(b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", mw.Boundary())
	b.Write(body.Bytes())
}

func writeBodyParts(mw *multipart.Writer, m *Message) {
	text, _ := mw.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=utf-8"},
		"Content-Transfer-Encoding": {"quoted-printable"},
	})
	fmt.Fprint(text, qp(m.Text))
	if m.HTML != "" {
		html, _ := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {"text/html; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		fmt.Fprint(html, qp(m.HTML))
	}
}

func writeMixed(b *strings.Builder, m *Message) error {
	var body bytes.Buffer
	outer := multipart.NewWriter(&body)

	if m.HTML != "" {
		var alt bytes.Buffer
		inner := multipart.NewWriter(&alt)
		writeBodyParts(inner, m)
		inner.Close()
		part, _ := outer.CreatePart(textproto.MIMEHeader{
			"Content-Type": {mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": inner.Boundary()})},
		})
		part.Write(alt.Bytes())
	} else {
		writeBodyParts(outer, m)
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
		part.Write([]byte(wrapBase64(a.Content)))
	}
	outer.Close()
	fmt.Fprintf(b, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", outer.Boundary())
	b.Write(body.Bytes())
	return nil
}

func qp(s string) string {
	var b strings.Builder
	w := quotedprintable.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.String()
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
