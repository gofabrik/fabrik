package mail_test

import (
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/mail"
)

func valid() mail.Message {
	return mail.Message{
		From:    "noreply@fabrik.test",
		To:      []string{"ada@example.com"},
		Subject: "Hello",
		Text:    "Hi!",
	}
}

func TestValidate_Accepts(t *testing.T) {
	cases := map[string]func(*mail.Message){
		"minimal":      func(*mail.Message) {},
		"display name": func(m *mail.Message) { m.From = `"Fabrik Café" <noreply@fabrik.test>` },
		"cc only":      func(m *mail.Message) { m.To = nil; m.Cc = []string{"ops@example.com"} },
		"bcc only":     func(m *mail.Message) { m.To = nil; m.Bcc = []string{"archive@example.com"} },
		"message id":   func(m *mail.Message) { m.ID = "welcome-42@fabrik.test" },
		"dotted id":    func(m *mail.Message) { m.ID = "a.b.c@d.e" },
		"reply to":     func(m *mail.Message) { m.ReplyTo = "support@fabrik.test" },
		"attachment":   func(m *mail.Message) { m.Attachments = []mail.Attachment{{Filename: "a.txt", Content: []byte("x")}} },
		"typed content": func(m *mail.Message) {
			m.Attachments = []mail.Attachment{{Filename: "a", ContentType: "text/plain; charset=utf-8", Content: []byte("x")}}
		},
		"html alternate": func(m *mail.Message) { m.HTML = "<p>Hi</p>" },
	}
	for name, mutate := range cases {
		m := valid()
		mutate(&m)
		if err := m.Validate(); err != nil {
			t.Errorf("%s: Validate() = %v, want nil", name, err)
		}
	}
}

func TestValidate_Rejects(t *testing.T) {
	cases := map[string]func(*mail.Message){
		"missing from":        func(m *mail.Message) { m.From = "" },
		"bad from":            func(m *mail.Message) { m.From = "not-an-address" },
		"no recipients":       func(m *mail.Message) { m.To = nil },
		"bad to":              func(m *mail.Message) { m.To = []string{"nope"} },
		"bad cc":              func(m *mail.Message) { m.Cc = []string{"nope"} },
		"bad bcc":             func(m *mail.Message) { m.Bcc = []string{"nope"} },
		"bad reply-to":        func(m *mail.Message) { m.ReplyTo = "nope" },
		"missing subject":     func(m *mail.Message) { m.Subject = "" },
		"multiline subject":   func(m *mail.Message) { m.Subject = "a\r\nBcc: evil@x" },
		"blank text":          func(m *mail.Message) { m.Text = "  \n" },
		"id without at":       func(m *mail.Message) { m.ID = "foo" },
		"id two ats":          func(m *mail.Message) { m.ID = "a@b@c" },
		"id empty left":       func(m *mail.Message) { m.ID = "@b" },
		"id empty right":      func(m *mail.Message) { m.ID = "a@" },
		"id bracket":          func(m *mail.Message) { m.ID = "<a@b>" },
		"id space":            func(m *mail.Message) { m.ID = "a b@c" },
		"id colon":            func(m *mail.Message) { m.ID = "foo:bar@c" },
		"unicode local from":  func(m *mail.Message) { m.From = "józsef@example.com" },
		"unicode local to":    func(m *mail.Message) { m.To = []string{"józsef@example.com"} },
		"unicode local cc":    func(m *mail.Message) { m.Cc = []string{"józsef@example.com"} },
		"unicode local bcc":   func(m *mail.Message) { m.Bcc = []string{"józsef@example.com"} },
		"unicode domain from": func(m *mail.Message) { m.From = "user@例え.jp" },
		"unicode domain to":   func(m *mail.Message) { m.To = []string{"user@例え.jp"} },
		"unicode domain cc":   func(m *mail.Message) { m.Cc = []string{"user@例え.jp"} },
		"unicode domain bcc":  func(m *mail.Message) { m.Bcc = []string{"user@例え.jp"} },
		"attachment no name":  func(m *mail.Message) { m.Attachments = []mail.Attachment{{Content: []byte("x")}} },
		"attachment bad type": func(m *mail.Message) {
			m.Attachments = []mail.Attachment{{Filename: "a", ContentType: "not a type;;;", Content: []byte("x")}}
		},
	}
	for name, mutate := range cases {
		m := valid()
		mutate(&m)
		err := m.Validate()
		if err == nil {
			t.Errorf("%s: Validate() = nil, want error", name)
			continue
		}
		if strings.HasPrefix(name, "unicode") && !strings.Contains(err.Error(), "punycode") {
			t.Errorf("%s: err = %v, want the punycode guidance", name, err)
		}
	}
}

func TestValidate_NilMessage(t *testing.T) {
	var m *mail.Message
	if err := m.Validate(); err == nil {
		t.Fatal("nil message must be an explicit error, not a panic")
	}
}
