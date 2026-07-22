package mail

import (
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"
)

var wireNow = time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)

func wireOf(t *testing.T, m *Message) string {
	t.Helper()
	w, err := wireMessage(m, wireNow)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func parsed(t *testing.T, w string) *mail.Message {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(w))
	if err != nil {
		t.Fatalf("wire output does not parse as a message: %v\n%s", err, w)
	}
	return msg
}

func base() Message {
	return Message{
		From:    "noreply@fabrik.test",
		To:      []string{"ada@example.com"},
		Subject: "Hello",
		Text:    "Hi!",
	}
}

func TestWire_TextOnly(t *testing.T) {
	m := base()
	w := wireOf(t, &m)
	msg := parsed(t, w)
	if ct := msg.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	if te := msg.Header.Get("Content-Transfer-Encoding"); te != "quoted-printable" {
		t.Errorf("Content-Transfer-Encoding = %q", te)
	}
	body, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if string(body) != "Hi!" {
		t.Errorf("body = %q", body)
	}
	if msg.Header.Get("Date") == "" || msg.Header.Get("MIME-Version") != "1.0" {
		t.Error("missing Date or MIME-Version")
	}
}

func TestWire_AlternativeCarriesCTEOnEveryPart(t *testing.T) {
	m := base()
	m.HTML = "<p>Hi &amp; bye</p>"
	w := wireOf(t, &m)
	msg := parsed(t, w)
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("Content-Type = %v %v", mt, err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	want := []struct {
		contentType string
		body        string
	}{
		{"text/plain", m.Text},
		{"text/html", m.HTML},
	}
	for i, w := range want {
		// NextRawPart preserves Content-Transfer-Encoding, unlike NextPart.
		p, err := mr.NextRawPart()
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		if te := p.Header.Get("Content-Transfer-Encoding"); te != "quoted-printable" {
			t.Errorf("part %d Content-Transfer-Encoding = %q", i, te)
		}
		if ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type")); ct != w.contentType {
			t.Errorf("part %d Content-Type = %q, want %q", i, ct, w.contentType)
		}
		body, _ := io.ReadAll(quotedprintable.NewReader(p))
		if string(body) != w.body {
			t.Errorf("part %d body = %q, want %q", i, body, w.body)
		}
	}
	if _, err := mr.NextRawPart(); err != io.EOF {
		t.Fatalf("after both parts: %v, want io.EOF", err)
	}
}

func TestWire_MixedWithAttachment(t *testing.T) {
	m := base()
	m.HTML = "<p>Hi</p>"
	m.Attachments = []Attachment{{Filename: "café útmutató.txt", Content: []byte(strings.Repeat("x", 200))}}
	w := wireOf(t, &m)
	msg := parsed(t, w)
	mt, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mt != "multipart/mixed" {
		t.Fatalf("outer type = %q", mt)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	first, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	it, ip, _ := mime.ParseMediaType(first.Header.Get("Content-Type"))
	if it != "multipart/alternative" {
		t.Fatalf("first part = %q, want the alternative body", it)
	}
	inner := multipart.NewReader(first, ip["boundary"])
	wantLeaf := []struct {
		contentType string
		body        string
	}{
		{"text/plain", m.Text},
		{"text/html", m.HTML},
	}
	for i, w := range wantLeaf {
		p, err := inner.NextRawPart()
		if err != nil {
			t.Fatalf("nested part %d: %v", i, err)
		}
		if ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type")); ct != w.contentType {
			t.Errorf("nested part %d Content-Type = %q, want %q", i, ct, w.contentType)
		}
		if te := p.Header.Get("Content-Transfer-Encoding"); te != "quoted-printable" {
			t.Errorf("nested part %d Content-Transfer-Encoding = %q", i, te)
		}
		if body, _ := io.ReadAll(quotedprintable.NewReader(p)); string(body) != w.body {
			t.Errorf("nested part %d body = %q, want %q", i, body, w.body)
		}
	}
	if _, err := inner.NextRawPart(); err != io.EOF {
		t.Fatalf("after nested parts: %v, want io.EOF", err)
	}

	att, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if ct, _, _ := mime.ParseMediaType(att.Header.Get("Content-Type")); ct != "text/plain" {
		t.Errorf("detected attachment Content-Type = %q, want text/plain from content sniffing", ct)
	}
	if te := att.Header.Get("Content-Transfer-Encoding"); te != "base64" {
		t.Errorf("attachment CTE = %q", te)
	}
	disp := att.Header.Get("Content-Disposition")
	if _, dp, err := mime.ParseMediaType(disp); err != nil || dp["filename"] != "café útmutató.txt" {
		t.Errorf("Content-Disposition = %q (params %v, err %v); non-ASCII filename must round-trip", disp, dp, err)
	}
	raw := rawPart(t, w, "base64")
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(raw, "\r\n", ""))
	if err != nil || string(decoded) != strings.Repeat("x", 200) {
		t.Errorf("attachment payload does not round-trip (err %v)", err)
	}
	for _, line := range strings.Split(raw, "\r\n") {
		if len(line) > 76 {
			t.Errorf("base64 line %d chars, want <= 76", len(line))
		}
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatalf("after attachment: %v, want io.EOF", err)
	}
}

func rawPart(t *testing.T, w, cte string) string {
	t.Helper()
	i := strings.Index(w, "Content-Transfer-Encoding: "+cte)
	if i < 0 {
		t.Fatalf("no %s part", cte)
	}
	rest := w[i:]
	j := strings.Index(rest, "\r\n\r\n")
	body := rest[j+4:]
	if k := strings.Index(body, "\r\n--"); k >= 0 {
		body = body[:k]
	}
	return body
}

func TestWire_EncodedHeadersAndFolding(t *testing.T) {
	m := base()
	m.From = `"Fabrik Café" <noreply@fabrik.test>`
	m.Subject = "Egy nagyon hosszú tárgy mező ami biztosan túl fog nyúlni a hetvennyolc karakteres soron és hajtogatni kell"
	w := wireOf(t, &m)
	msg := parsed(t, w)
	dec := new(mime.WordDecoder)
	from, err := dec.DecodeHeader(msg.Header.Get("From"))
	if err != nil || !strings.Contains(from, "Fabrik Café") {
		t.Errorf("From = %q err=%v", from, err)
	}
	subj, err := dec.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subj != m.Subject {
		t.Errorf("Subject must round-trip in full:\n got %q\nwant %q (err %v)", subj, m.Subject, err)
	}
	for _, line := range strings.Split(w, "\r\n") {
		if len(line) > 78 {
			t.Errorf("header/body line exceeds 78 chars: %q", line)
		}
		if line == "" {
			break
		}
	}
}

func TestWire_BccAndEmptyHeaderOmission(t *testing.T) {
	m := base()
	m.To = nil
	m.Bcc = []string{"archive@example.com"}
	w := wireOf(t, &m)
	if strings.Contains(w, "Bcc:") {
		t.Error("Bcc must never appear in headers")
	}
	if strings.Contains(w, "To:") || strings.Contains(w, "Cc:") {
		t.Error("empty To/Cc headers must be omitted")
	}

	m2 := base()
	m2.To = nil
	m2.Cc = []string{"ops@example.com"}
	w2 := wireOf(t, &m2)
	ccMsg := parsed(t, w2)
	if got, err := ccMsg.Header.AddressList("Cc"); err != nil || len(got) != 1 || got[0].Address != "ops@example.com" {
		t.Errorf("Cc-only message must carry its Cc header (got %v, %v):\n%s", got, err, w2)
	}
	if strings.Contains(w2, "To:") {
		t.Error("empty To header must be omitted")
	}
}

func TestWire_MessageIDAndReplyTo(t *testing.T) {
	m := base()
	m.ID = "welcome-42@fabrik.test"
	m.ReplyTo = "support@fabrik.test"
	w := wireOf(t, &m)
	msg := parsed(t, w)
	if got := msg.Header.Get("Message-ID"); got != "<welcome-42@fabrik.test>" {
		t.Errorf("Message-ID = %q", got)
	}
	if got, err := msg.Header.AddressList("Reply-To"); err != nil || len(got) != 1 || got[0].Address != "support@fabrik.test" {
		t.Errorf("Reply-To = %v (%v)", got, err)
	}
}

func TestWire_InvalidMessageRejected(t *testing.T) {
	m := base()
	m.Subject = "evil\r\nBcc: x@y"
	if _, err := wireMessage(&m, wireNow); err == nil {
		t.Fatal("wireMessage must reject what Validate rejects")
	}
}
