package mail_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/mail"
	"github.com/gofabrik/fabrik/mail/transporttest"
)

func TestLog_Conformance(t *testing.T) {
	transporttest.Run(t, func() mail.Transport {
		return &mail.Log{Logger: slog.New(slog.DiscardHandler)}
	})
}

func TestLog_EmitsStructuredRecord(t *testing.T) {
	var buf bytes.Buffer
	l := &mail.Log{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	m := valid()
	m.HTML = "<p>x</p>"
	if err := l.Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, needle := range []string{"mail", "noreply@fabrik.test", "ada@example.com", "Hello"} {
		if !strings.Contains(out, needle) {
			t.Errorf("log output missing %q:\n%s", needle, out)
		}
	}
}

func TestLog_NoEffectOnInvalidOrCanceled(t *testing.T) {
	var buf bytes.Buffer
	l := &mail.Log{Logger: slog.New(slog.NewTextHandler(&buf, nil))}

	m := valid()
	m.From = ""
	if err := l.Send(context.Background(), &m); err == nil {
		t.Fatal("invalid message accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	good := valid()
	if err := l.Send(ctx, &good); err == nil {
		t.Fatal("pre-canceled context accepted")
	}
	if buf.Len() != 0 {
		t.Errorf("rejected sends must log nothing, got:\n%s", buf.String())
	}
}

func TestLog_NilLoggerUsesDefault(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	m := valid()
	if err := (&mail.Log{}).Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Hello") {
		t.Errorf("default logger not used:\n%s", buf.String())
	}
}
