package mail_test

import (
	"context"
	"sync"
	"testing"

	"github.com/gofabrik/fabrik/mail"
	"github.com/gofabrik/fabrik/mail/transporttest"
)

func TestRecorder_Conformance(t *testing.T) {
	transporttest.Run(t, func() mail.Transport { return &mail.Recorder{} })
}

func TestRecorder_DeepIsolationBothDirections(t *testing.T) {
	rec := &mail.Recorder{}
	m := valid()
	m.Cc = []string{"ops@example.com"}
	m.Attachments = []mail.Attachment{{Filename: "a.txt", Content: []byte("orig")}}
	if err := rec.Send(context.Background(), &m); err != nil {
		t.Fatal(err)
	}

	m.To[0] = "mallory@evil.example"
	m.Attachments[0].Content[0] = 'X'
	if got := rec.Sent()[0]; got.To[0] != "ada@example.com" || string(got.Attachments[0].Content) != "orig" {
		t.Fatalf("caller mutation reached the record: %+v", got)
	}

	out := rec.Sent()
	out[0].To[0] = "tamper@evil.example"
	out[0].Attachments[0].Content[0] = 'Y'
	if got := rec.Sent()[0]; got.To[0] != "ada@example.com" || string(got.Attachments[0].Content) != "orig" {
		t.Fatalf("mutating Sent() results reached the record: %+v", got)
	}
}

func TestRecorder_NoRecordOnInvalidOrCanceled(t *testing.T) {
	rec := &mail.Recorder{}
	bad := valid()
	bad.From = ""
	rec.Send(context.Background(), &bad)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	good := valid()
	rec.Send(ctx, &good)
	if n := len(rec.Sent()); n != 0 {
		t.Fatalf("rejected sends recorded %d messages", n)
	}
}

func TestRecorder_ConcurrentSends(t *testing.T) {
	rec := &mail.Recorder{}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := valid()
			if err := rec.Send(context.Background(), &m); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := len(rec.Sent()); n != 20 {
		t.Fatalf("recorded %d, want 20", n)
	}
}
