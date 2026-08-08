package mail_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/mail"
	"github.com/gofabrik/fabrik/mail/transporttest"
)

func TestDev_Conformance(t *testing.T) {
	transporttest.Run(t, func() mail.Transport {
		return &mail.Dev{Output: io.Discard, Logger: slog.New(slog.DiscardHandler)}
	})
}

func TestDev_RendersReadablePreview(t *testing.T) {
	var output bytes.Buffer
	m := &mail.Message{
		From:    "Fabrik <noreply@fabrik.test>",
		To:      []string{"ada@example.com"},
		Subject: "Confirm your account",
		Text:    "Hello Ada,\n\nConfirm at https://example.com/confirm/token-123\n",
		HTML:    "<p>Open the link.</p>",
		Attachments: []mail.Attachment{
			{Filename: "guide.txt", Content: []byte("read me")},
		},
	}
	dev := &mail.Dev{
		Output: &output,
		Logger: slog.New(slog.DiscardHandler),
	}

	if err := dev.Send(context.Background(), m); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	for _, want := range []string{
		"--- mail ---\n",
		"subject: Confirm your account\n",
		"from: Fabrik <noreply@fabrik.test>\n",
		"to: ada@example.com\n",
		m.Text,
		"html: 21 bytes\n",
		"attachment: guide.txt (7 bytes)\n",
		"link: https://example.com/confirm/token-123\n",
		"--- end mail ---\n",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("preview missing %q:\n%s", want, output.String())
		}
	}
}

func TestDev_NilLoggerUsesDefault(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	m := valid()
	dev := &mail.Dev{Output: io.Discard}
	if err := dev.Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Hello") {
		t.Errorf("default logger not used:\n%s", buf.String())
	}
}

func TestDev_NilOutputWritesToStdout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = prev }()

	m := valid()
	if err := (&mail.Dev{Logger: discardLogger()}).Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	w.Close()
	os.Stdout = prev
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "--- mail ---") {
		t.Errorf("nil Output should write the preview to stdout:\n%s", out)
	}
}

func TestDev_OneLinkLinePerURL(t *testing.T) {
	m := valid()
	m.Text = "First https://a.example/one then https://b.example/two here."
	var output bytes.Buffer
	if err := (&mail.Dev{Output: &output, Logger: discardLogger()}).Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(output.String(), "link: "); got != 2 {
		t.Errorf("link lines = %d, want one per URL (2):\n%s", got, output.String())
	}
	for _, want := range []string{"link: https://a.example/one", "link: https://b.example/two"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("preview missing %q", want)
		}
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDev_PreviewsAllRecipientGroups(t *testing.T) {
	m := valid()
	m.Cc = []string{"cc@example.test"}
	m.Bcc = []string{"bcc@example.test"}
	var output bytes.Buffer
	if err := (&mail.Dev{Output: &output, Logger: discardLogger()}).Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cc: cc@example.test", "bcc: bcc@example.test"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("preview missing %q:\n%s", want, output.String())
		}
	}
}

func TestDev_LinksDropSentencePunctuation(t *testing.T) {
	m := valid()
	m.Text = "Visit https://example.test/reset. Or (https://example.test/alt): " +
		"see https://en.wikipedia.org/wiki/Function_(mathematics) and " +
		"(https://example.test/wrapped)., done https://example.test/plain"
	var output bytes.Buffer
	if err := (&mail.Dev{Output: &output, Logger: discardLogger()}).Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"link: https://example.test/reset\n",
		"link: https://example.test/alt\n",
		"link: https://en.wikipedia.org/wiki/Function_(mathematics)\n",
		"link: https://example.test/wrapped\n",
		"link: https://example.test/plain\n",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("preview missing exact %q:\n%s", want, output.String())
		}
	}
}
