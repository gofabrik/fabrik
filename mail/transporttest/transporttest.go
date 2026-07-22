// Package transporttest checks the behavior shared by [mail.Transport]
// implementations.
package transporttest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gofabrik/fabrik/mail"
)

// Run tests transports returned by factory against the shared [mail.Transport]
// contract.
func Run(t *testing.T, factory func() mail.Transport) {
	t.Helper()

	valid := func() *mail.Message {
		return &mail.Message{
			From:    "noreply@fabrik.test",
			To:      []string{"ada@example.com"},
			Subject: "Hello",
			Text:    "Hi!",
		}
	}

	t.Run("NilMessage", func(t *testing.T) {
		if err := factory().Send(context.Background(), nil); err == nil {
			t.Fatal("Send(nil) = nil error")
		}
	})

	t.Run("InvalidMessage", func(t *testing.T) {
		m := valid()
		m.From = ""
		if err := factory().Send(context.Background(), m); err == nil {
			t.Fatal("Send of invalid message = nil error")
		}
	})

	t.Run("PreCanceledContext", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := factory().Send(ctx, valid())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send with pre-canceled context = %v, want ctx.Err()", err)
		}
	})

	t.Run("InputNotMutated", func(t *testing.T) {
		m := valid()
		m.Cc = []string{"ops@example.com"}
		m.Attachments = []mail.Attachment{{Filename: "a.txt", Content: []byte("x")}}
		snapshot := clone(m)
		if err := factory().Send(context.Background(), m); err != nil {
			t.Fatalf("valid send failed: %v", err)
		}
		if !reflect.DeepEqual(*m, snapshot) {
			t.Fatalf("Send mutated the message:\n got %+v\nwant %+v", *m, snapshot)
		}
	})
}

func clone(m *mail.Message) mail.Message {
	out := *m
	out.To = append([]string(nil), m.To...)
	out.Cc = append([]string(nil), m.Cc...)
	out.Bcc = append([]string(nil), m.Bcc...)
	out.Attachments = append([]mail.Attachment(nil), m.Attachments...)
	for i := range out.Attachments {
		out.Attachments[i].Content = append([]byte(nil), m.Attachments[i].Content...)
	}
	return out
}
