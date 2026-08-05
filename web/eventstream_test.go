package web_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/web"
)

// streamWriter supports what a real connection supports, and records it, which
// httptest.ResponseRecorder alone cannot: it has no write deadline.
type streamWriter struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
	flushes   int
	writeErr  error
}

func newStreamWriter() *streamWriter {
	return &streamWriter{ResponseRecorder: httptest.NewRecorder()}
}

func (s *streamWriter) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadlines = append(s.deadlines, t)
	return nil
}

func (s *streamWriter) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushes++
	s.ResponseRecorder.Flush()
}

// WriteString has to funnel through Write: io.WriteString prefers it, and the
// embedded recorder supplies one that would bypass the failure this injects.
func (s *streamWriter) WriteString(str string) (int, error) {
	return s.Write([]byte(str))
}

func (s *streamWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.ResponseRecorder.Write(b)
}

func (s *streamWriter) body() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ResponseRecorder.Body.String()
}

func (s *streamWriter) counts() (deadlines, flushes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deadlines), s.flushes
}

// run responds on this goroutine, so every assertion afterwards sees a
// finished stream.
func run(t *testing.T, w http.ResponseWriter, r *http.Request, resp web.Response) {
	t.Helper()
	if err := resp.Respond(w, r); err != nil {
		t.Fatalf("Respond: %v", err)
	}
}

func TestEventStreamFraming(t *testing.T) {
	for _, tt := range []struct {
		name  string
		event string
		want  string
	}{
		{"plain", "hello", "data: hello\n\n"},
		{"embedded LF", "one\ntwo", "data: one\ndata: two\n\n"},
		{"embedded CRLF", "one\r\ntwo", "data: one\ndata: two\n\n"},
		{"bare CR", "one\rtwo", "data: one\ndata: two\n\n"},
		{"empty", "", "data: \n\n"},
		{"a line that looks like a field", "event: nope", "data: event: nope\n\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			events := make(chan string, 1)
			events <- tt.event
			close(events)

			w := newStreamWriter()
			run(t, w, httptest.NewRequest("GET", "/", nil), web.EventStream(events))

			if got := w.body(); got != tt.want {
				t.Errorf("frame = %q, want %q", got, tt.want)
			}
		})
	}
}

// Data can carry anything, including something shaped like another event. It
// must arrive as one event whose data has newlines in it, never as two.
func TestEventStreamCannotForgeAField(t *testing.T) {
	events := make(chan string, 1)
	events <- "harmless\n\nid: 7\nevent: stolen\ndata: forged"
	close(events)

	w := newStreamWriter()
	run(t, w, httptest.NewRequest("GET", "/", nil), web.EventStream(events))

	body := w.body()
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n") {
		if line != "" && !strings.HasPrefix(line, "data: ") {
			t.Fatalf("line %q escaped the data field; body = %q", line, body)
		}
	}
	if strings.Count(body, "\n\n") != 1 {
		t.Errorf("body ended %d events, want 1: %q", strings.Count(body, "\n\n"), body)
	}
}

func TestEventStreamHeadersAndStatus(t *testing.T) {
	events := make(chan string)
	close(events)

	w := newStreamWriter()
	run(t, w, httptest.NewRequest("GET", "/", nil),
		web.EventStream(events).Header("X-Kind", "logs"))

	for key, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
		"X-Kind":            "logs",
	} {
		if got := w.Header().Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if w.Code != http.StatusOK {
		t.Errorf("code = %d", w.Code)
	}
}

func TestEventStreamInitialComesFirstAndInOrder(t *testing.T) {
	events := make(chan string, 1)
	events <- "live"
	close(events)

	w := newStreamWriter()
	run(t, w, httptest.NewRequest("GET", "/", nil),
		web.EventStream(events).Initial("first", "second"))

	want := "data: first\n\ndata: second\n\ndata: live\n\n"
	if got := w.body(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// The caller's slice and the response must not be able to change each other.
func TestEventStreamInitialIsCopied(t *testing.T) {
	backlog := []string{"one"}
	events := make(chan string)
	close(events)

	resp := web.EventStream(events).Initial(backlog...)
	backlog[0] = "changed"

	w := newStreamWriter()
	run(t, w, httptest.NewRequest("GET", "/", nil), resp)

	if got := w.body(); got != "data: one\n\n" {
		t.Errorf("body = %q, want the value as it was when Initial was called", got)
	}
}

func TestEventStreamEndsWhenTheChannelCloses(t *testing.T) {
	events := make(chan string, 1)
	events <- "last"
	close(events)

	closed := 0
	w := newStreamWriter()
	run(t, w, httptest.NewRequest("GET", "/", nil),
		web.EventStream(events).OnClose(func() { closed++ }))

	if closed != 1 {
		t.Errorf("OnClose ran %d times, want once", closed)
	}
	if got := w.body(); got != "data: last\n\n" {
		t.Errorf("body = %q", got)
	}
}

func TestEventStreamEndsWhenTheRequestIsCancelled(t *testing.T) {
	events := make(chan string) // never closed, never written
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)

	closed := make(chan struct{})
	go func() {
		w := newStreamWriter()
		_ = web.EventStream(events).OnClose(func() { close(closed) }).Respond(w, r)
	}()

	cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled request should end the stream")
	}
}

func TestEventStreamEndsWhenTheWriteFails(t *testing.T) {
	events := make(chan string, 1)
	events <- "doomed"

	w := newStreamWriter()
	w.writeErr = errors.New("client went away")

	closed := 0
	// Respond must return, not block on the channel that is still open.
	run(t, w, httptest.NewRequest("GET", "/", nil),
		web.EventStream(events).OnClose(func() { closed++ }))

	if closed != 1 {
		t.Errorf("OnClose ran %d times, want once", closed)
	}
}

// A deadline is set for every write rather than once for the stream, which is
// what lets a stream outlive it while still bounding any single write.
func TestEventStreamDeadlineIsSetPerWrite(t *testing.T) {
	events := make(chan string, 3)
	for _, e := range []string{"one", "two", "three"} {
		events <- e
	}
	close(events)

	w := newStreamWriter()
	run(t, w, httptest.NewRequest("GET", "/", nil),
		web.EventStream(events).WriteTimeout(time.Second))

	// The handshake flush is one write of its own, then one per event.
	deadlines, flushes := w.counts()
	if deadlines != 4 {
		t.Errorf("deadlines set = %d, want the handshake plus one per event", deadlines)
	}
	if flushes != 4 {
		t.Errorf("flushes = %d, want the handshake plus one per event", flushes)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	for i := 1; i < len(w.deadlines); i++ {
		if !w.deadlines[i].After(w.deadlines[i-1]) && w.deadlines[i] != w.deadlines[i-1] {
			t.Errorf("deadline %d went backwards", i)
		}
	}
}

// A writer with no deadline or flush support is not a reason to hang up:
// plainWriter (web_test.go) offers neither, so ResponseController reports
// errors.ErrUnsupported for both and the stream still delivers.
func TestEventStreamToleratesAPlainWriter(t *testing.T) {
	events := make(chan string, 2)
	events <- "hello"
	events <- "again"
	close(events)

	w := &plainWriter{h: http.Header{}}
	run(t, w, httptest.NewRequest("GET", "/", nil), web.EventStream(events))

	// Both events, not just the first: an unsupported flush must not end
	// the stream after one send.
	if got := string(w.body); got != "data: hello\n\ndata: again\n\n" {
		t.Errorf("body = %q, want both events delivered", got)
	}
}

func TestEventStreamHeartbeat(t *testing.T) {
	events := make(chan string) // silent
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)

	w := newStreamWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = web.EventStream(events).Heartbeat(20*time.Millisecond).Respond(w, r)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if strings.Count(w.body(), ":\n\n") >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("want at least two heartbeats, got %q", w.body())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

// The default write deadline is what EventStream promises when nothing was
// chained: ten seconds out from each write.
func TestEventStreamDefaultWriteTimeout(t *testing.T) {
	events := make(chan string, 1)
	events <- "one"
	close(events)

	w := newStreamWriter()
	before := time.Now()
	run(t, w, httptest.NewRequest("GET", "/", nil), web.EventStream(events))

	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.deadlines) != 2 {
		t.Fatalf("deadlines set = %d, want the handshake plus one event", len(w.deadlines))
	}
	for i, dl := range w.deadlines {
		d := dl.Sub(before)
		if d < 9*time.Second || d > 11*time.Second {
			t.Errorf("deadline %d = %v out, want about 10s", i, d)
		}
	}
}

// No heartbeat was asked for, so none may arrive: an idle stream writes
// nothing at all.
func TestEventStreamHeartbeatOffByDefault(t *testing.T) {
	events := make(chan string) // silent
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)

	w := newStreamWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = web.EventStream(events).Respond(w, r)
	}()

	time.Sleep(100 * time.Millisecond)
	if got := w.body(); got != "" {
		t.Errorf("an idle default stream wrote %q, want nothing", got)
	}
	cancel()
	<-done
}

// Chaining returns copies, so two streams built from one base do not share
// their settings.
func TestEventStreamChainingDoesNotMutate(t *testing.T) {
	events := make(chan string)
	close(events)

	base := web.EventStream(events)
	withInitial := base.Initial("only here")

	w := newStreamWriter()
	run(t, w, httptest.NewRequest("GET", "/", nil), base)
	if got := w.body(); got != "" {
		t.Errorf("the base sent %q, want nothing", got)
	}

	w2 := newStreamWriter()
	run(t, w2, httptest.NewRequest("GET", "/", nil), withInitial)
	if got := w2.body(); got != "data: only here\n\n" {
		t.Errorf("the copy sent %q", got)
	}
}

// An EventSource is not open until the headers arrive, and a silent stream
// queues no frame to carry them: the handshake must flush on its own.
func TestEventStreamFlushesTheHandshake(t *testing.T) {
	events := make(chan string) // silent
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/", nil).WithContext(ctx)

	w := newStreamWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = web.EventStream(events).Respond(w, r)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if _, flushes := w.counts(); flushes >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("headers were never flushed for a silent stream")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}
