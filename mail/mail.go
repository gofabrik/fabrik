// Package mail composes and delivers transactional email.
package mail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	netmail "net/mail"
	"strings"
)

// Renderer renders a named template to a writer.
type Renderer interface {
	Render(w io.Writer, template string, data any) error
}

// Attachment is a file attached to a message.
type Attachment struct {
	Filename    string
	ContentType string // detected from content when empty
	Content     []byte
}

// Message is an outbound email whose address parts must be ASCII, although
// display names may contain UTF-8; ID provides a stable Message-ID for retries.
type Message struct {
	ID          string // optional dot-atom@dot-atom Message-ID
	From        string
	To          []string
	Cc          []string
	Bcc         []string
	ReplyTo     string
	Subject     string
	Text        string // required plain-text body
	HTML        string // optional HTML alternative
	Attachments []Attachment
}

// Render fills Text and HTML atomically; an empty htmlTemplate leaves HTML empty.
func (m *Message) Render(renderer Renderer, textTemplate, htmlTemplate string, data any) error {
	if m == nil {
		return fmt.Errorf("mail: render on nil message")
	}
	if renderer == nil {
		return fmt.Errorf("mail: nil renderer")
	}
	var text, html bytes.Buffer
	if err := renderer.Render(&text, textTemplate, data); err != nil {
		return fmt.Errorf("mail: text body: %w", err)
	}
	if htmlTemplate != "" {
		if err := renderer.Render(&html, htmlTemplate, data); err != nil {
			return fmt.Errorf("mail: html body: %w", err)
		}
	}
	m.Text = text.String()
	m.HTML = html.String()
	return nil
}

// Validate reports the first message validation error.
func (m *Message) Validate() error {
	if m == nil {
		return fmt.Errorf("mail: nil message")
	}
	if m.From == "" {
		return fmt.Errorf("mail: message needs From")
	}
	if err := validAddress(m.From); err != nil {
		return err
	}
	if len(m.To)+len(m.Cc)+len(m.Bcc) == 0 {
		return fmt.Errorf("mail: message needs at least one recipient")
	}
	for _, group := range [][]string{m.To, m.Cc, m.Bcc} {
		for _, addr := range group {
			if err := validAddress(addr); err != nil {
				return err
			}
		}
	}
	if m.ReplyTo != "" {
		if err := validAddress(m.ReplyTo); err != nil {
			return err
		}
	}
	if m.Subject == "" {
		return fmt.Errorf("mail: message needs Subject")
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		return fmt.Errorf("mail: Subject must be a single line")
	}
	if strings.TrimSpace(m.Text) == "" {
		return fmt.Errorf("mail: message needs a Text body")
	}
	if m.ID != "" {
		if err := validMessageID(m.ID); err != nil {
			return err
		}
	}
	for _, a := range m.Attachments {
		if a.Filename == "" {
			return fmt.Errorf("mail: attachment needs a filename")
		}
		if a.ContentType != "" {
			if _, _, err := mime.ParseMediaType(a.ContentType); err != nil {
				return fmt.Errorf("mail: attachment %q content type: %w", a.Filename, err)
			}
		}
	}
	return nil
}

// validAddress rejects non-ASCII address parts but permits UTF-8 display names.
func validAddress(addr string) error {
	a, err := netmail.ParseAddress(addr)
	if err != nil {
		return fmt.Errorf("mail: invalid address %q: %w", addr, err)
	}
	for i := 0; i < len(a.Address); i++ {
		if a.Address[i] >= 0x80 {
			return fmt.Errorf("mail: address %q is not ASCII; use punycode for internationalized domains", a.Address)
		}
	}
	return nil
}

func validMessageID(id string) error {
	local, domain, ok := strings.Cut(id, "@")
	if !ok || strings.Contains(domain, "@") || !dotAtom(local) || !dotAtom(domain) {
		return fmt.Errorf("mail: invalid ID %q: want dot-atom@dot-atom", id)
	}
	return nil
}

func dotAtom(s string) bool {
	for _, run := range strings.Split(s, ".") {
		if run == "" {
			return false
		}
		for i := 0; i < len(run); i++ {
			if !atext(run[i]) {
				return false
			}
		}
	}
	return true
}

func atext(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-/=?^_`{|}~", b) >= 0
}

func (m *Message) recipients() []string {
	out := make([]string, 0, len(m.To)+len(m.Cc)+len(m.Bcc))
	out = append(out, m.To...)
	out = append(out, m.Cc...)
	out = append(out, m.Bcc...)
	return out
}

// Transport synchronously delivers messages without retaining or mutating them;
// pre-canceled contexts fail before side effects, and acceptance is final despite
// later cleanup errors or cancellation.
type Transport interface {
	Send(ctx context.Context, m *Message) error
}

func cloneMessage(m *Message) Message {
	out := *m
	out.To = append([]string(nil), m.To...)
	out.Cc = append([]string(nil), m.Cc...)
	out.Bcc = append([]string(nil), m.Bcc...)
	out.Attachments = append([]Attachment(nil), m.Attachments...)
	for i := range out.Attachments {
		out.Attachments[i].Content = append([]byte(nil), m.Attachments[i].Content...)
	}
	return out
}
