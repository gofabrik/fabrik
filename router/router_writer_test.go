package router

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type dCapBase struct {
	http.ResponseWriter
	flushed    int
	hijackErr  error
	hijackConn net.Conn
	hijackRW   *bufio.ReadWriter
	pushed     []string
	pushOpts   []*http.PushOptions
	pushErr    error
	readFrom   bytes.Buffer
	rfSrc      io.Reader
	feErr      error
	rfN        int64
	rfErr      error
}

func (b *dCapBase) Flush() { b.flushed++ }

func (b *dCapBase) FlushError() error { b.flushed++; return b.feErr }

func (b *dCapBase) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if b.hijackErr != nil {
		return nil, nil, b.hijackErr
	}
	if b.hijackConn == nil {
		b.hijackConn, _ = net.Pipe()
		b.hijackRW = bufio.NewReadWriter(bufio.NewReader(b.hijackConn), bufio.NewWriter(b.hijackConn))
	}
	return b.hijackConn, b.hijackRW, nil
}

func (b *dCapBase) Push(target string, opts *http.PushOptions) error {
	b.pushed = append(b.pushed, target)
	b.pushOpts = append(b.pushOpts, opts)
	return b.pushErr
}

func (b *dCapBase) ReadFrom(src io.Reader) (int64, error) {
	b.rfSrc = src
	if b.rfErr != nil {
		return b.rfN, b.rfErr
	}
	return io.Copy(&b.readFrom, src)
}

func newDCapPair() (*dCapBase, *defaultStatusWriter, http.ResponseWriter) {
	base := &dCapBase{ResponseWriter: httptest.NewRecorder()}
	dw := &defaultStatusWriter{ResponseWriter: base, code: http.StatusNotFound}
	return base, dw, wrapDefaultStatusWriter(dw)
}

func TestDefaultStatusWriterCapabilityBehaviorForwarding(t *testing.T) {
	base, _, w := newDCapPair()

	w.(http.Flusher).Flush()
	if base.flushed != 1 {
		t.Fatalf("flush not forwarded, count=%d", base.flushed)
	}

	pushErr := errors.New("push refused")
	base.pushErr = pushErr
	opts := &http.PushOptions{Method: "GET"}
	if err := w.(http.Pusher).Push("/style.css", opts); err != pushErr {
		t.Fatalf("push error not forwarded: %v", err)
	}
	if len(base.pushed) != 1 || base.pushed[0] != "/style.css" {
		t.Fatalf("push target not forwarded: %v", base.pushed)
	}
	if len(base.pushOpts) != 1 || base.pushOpts[0] != opts {
		t.Fatalf("push options not forwarded by identity: %v", base.pushOpts)
	}

	base2, _, w2 := newDCapPair()
	src := strings.NewReader("payload")
	n, err := w2.(io.ReaderFrom).ReadFrom(src)
	if err != nil || n != int64(len("payload")) || base2.readFrom.String() != "payload" {
		t.Fatalf("readFrom not forwarded: n=%d err=%v got=%q", n, err, base2.readFrom.String())
	}
	if base2.rfSrc != src {
		t.Fatal("readFrom source not forwarded by identity")
	}

	rfErr := errors.New("read refused")
	base2.rfErr = rfErr
	base2.rfN = 7
	if n, err := w2.(io.ReaderFrom).ReadFrom(strings.NewReader("x")); n != 7 || err != rfErr {
		t.Fatalf("readFrom result tuple not forwarded: (%d, %v), want (7, %v)", n, err, rfErr)
	}

	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil || conn != base.hijackConn || rw != base.hijackRW {
		t.Fatalf("hijack results not forwarded by identity: %v", err)
	}
	conn.Close()
}

func TestFailedHijackDefaultStatusWriter(t *testing.T) {
	base, _, w := newDCapPair()
	base.hijackErr = errors.New("no hijack")

	if _, _, err := w.(http.Hijacker).Hijack(); err != base.hijackErr {
		t.Fatalf("hijack error not forwarded by identity: %v", err)
	}
}

type dBaseWriter struct{}

func (dBaseWriter) Header() http.Header       { return nil }
func (dBaseWriter) Write([]byte) (int, error) { return 0, nil }
func (dBaseWriter) WriteHeader(int)           {}

type dmF struct{}

func (dmF) Flush() {}

type dmH struct{}

func (dmH) Hijack() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, nil }

type dmP struct{}

func (dmP) Push(string, *http.PushOptions) error { return nil }

type dmR struct{}

func (dmR) ReadFrom(io.Reader) (int64, error) { return 0, nil }

type (
	dbF struct {
		dBaseWriter
		dmF
	}
	dbH struct {
		dBaseWriter
		dmH
	}
	dbP struct {
		dBaseWriter
		dmP
	}
	dbR struct {
		dBaseWriter
		dmR
	}
	dbFH struct {
		dBaseWriter
		dmF
		dmH
	}
	dbFP struct {
		dBaseWriter
		dmF
		dmP
	}
	dbFR struct {
		dBaseWriter
		dmF
		dmR
	}
	dbHP struct {
		dBaseWriter
		dmH
		dmP
	}
	dbHR struct {
		dBaseWriter
		dmH
		dmR
	}
	dbPR struct {
		dBaseWriter
		dmP
		dmR
	}
	dbFHP struct {
		dBaseWriter
		dmF
		dmH
		dmP
	}
	dbFHR struct {
		dBaseWriter
		dmF
		dmH
		dmR
	}
	dbFPR struct {
		dBaseWriter
		dmF
		dmP
		dmR
	}
	dbHPR struct {
		dBaseWriter
		dmH
		dmP
		dmR
	}
	dbFHPR struct {
		dBaseWriter
		dmF
		dmH
		dmP
		dmR
	}
)

type dWantCaps struct{ f, h, p, r bool }

func dAssertCaps(t *testing.T, name string, w http.ResponseWriter, want dWantCaps, base http.ResponseWriter) {
	t.Helper()
	if _, ok := w.(http.Flusher); ok != want.f {
		t.Errorf("%s: Flusher=%v, want %v", name, ok, want.f)
	}
	if _, ok := w.(http.Hijacker); ok != want.h {
		t.Errorf("%s: Hijacker=%v, want %v", name, ok, want.h)
	}
	if _, ok := w.(http.Pusher); ok != want.p {
		t.Errorf("%s: Pusher=%v, want %v", name, ok, want.p)
	}
	if _, ok := w.(io.ReaderFrom); ok != want.r {
		t.Errorf("%s: ReaderFrom=%v, want %v", name, ok, want.r)
	}
	u, ok := w.(interface{ Unwrap() http.ResponseWriter })
	if !ok || u.Unwrap() != base {
		t.Errorf("%s: Unwrap chain broken", name)
	}
}

func TestWrapDefaultStatusWriterSelection(t *testing.T) {
	cases := []struct {
		name string
		base http.ResponseWriter
		want dWantCaps
	}{
		{"none", dBaseWriter{}, dWantCaps{}},
		{"F", dbF{}, dWantCaps{f: true}},
		{"H", dbH{}, dWantCaps{h: true}},
		{"P", dbP{}, dWantCaps{p: true}},
		{"R", dbR{}, dWantCaps{r: true}},
		{"FH", dbFH{}, dWantCaps{f: true, h: true}},
		{"FP", dbFP{}, dWantCaps{f: true, p: true}},
		{"FR", dbFR{}, dWantCaps{f: true, r: true}},
		{"HP", dbHP{}, dWantCaps{h: true, p: true}},
		{"HR", dbHR{}, dWantCaps{h: true, r: true}},
		{"PR", dbPR{}, dWantCaps{p: true, r: true}},
		{"FHP", dbFHP{}, dWantCaps{f: true, h: true, p: true}},
		{"FHR", dbFHR{}, dWantCaps{f: true, h: true, r: true}},
		{"FPR", dbFPR{}, dWantCaps{f: true, p: true, r: true}},
		{"HPR", dbHPR{}, dWantCaps{h: true, p: true, r: true}},
		{"FHPR", dbFHPR{}, dWantCaps{f: true, h: true, p: true, r: true}},
	}
	for _, tc := range cases {
		dw := &defaultStatusWriter{ResponseWriter: tc.base, code: http.StatusNotFound}
		got := wrapDefaultStatusWriter(dw)
		dAssertCaps(t, tc.name, got, tc.want, tc.base)
	}
}

// These assertions cover every optional-interface combination, including Pusher combinations unavailable over HTTP/1.
var (
	_ http.Flusher = (*dwF)(nil)
	_ http.Flusher = (*dwFH)(nil)
	_ http.Flusher = (*dwFP)(nil)
	_ http.Flusher = (*dwFR)(nil)
	_ http.Flusher = (*dwFHP)(nil)
	_ http.Flusher = (*dwFHR)(nil)
	_ http.Flusher = (*dwFPR)(nil)
	_ http.Flusher = (*dwFHPR)(nil)

	_ http.Hijacker = (*dwH)(nil)
	_ http.Hijacker = (*dwFH)(nil)
	_ http.Hijacker = (*dwHP)(nil)
	_ http.Hijacker = (*dwHR)(nil)
	_ http.Hijacker = (*dwFHP)(nil)
	_ http.Hijacker = (*dwFHR)(nil)
	_ http.Hijacker = (*dwHPR)(nil)
	_ http.Hijacker = (*dwFHPR)(nil)

	_ http.Pusher = (*dwP)(nil)
	_ http.Pusher = (*dwFP)(nil)
	_ http.Pusher = (*dwHP)(nil)
	_ http.Pusher = (*dwPR)(nil)
	_ http.Pusher = (*dwFHP)(nil)
	_ http.Pusher = (*dwFPR)(nil)
	_ http.Pusher = (*dwHPR)(nil)
	_ http.Pusher = (*dwFHPR)(nil)

	_ io.ReaderFrom = (*dwR)(nil)
	_ io.ReaderFrom = (*dwFR)(nil)
	_ io.ReaderFrom = (*dwHR)(nil)
	_ io.ReaderFrom = (*dwPR)(nil)
	_ io.ReaderFrom = (*dwFHR)(nil)
	_ io.ReaderFrom = (*dwFPR)(nil)
	_ io.ReaderFrom = (*dwHPR)(nil)
	_ io.ReaderFrom = (*dwFHPR)(nil)
)

// ResponseController prefers FlushError, so wrappers must preserve its error.
func TestFlushErrorPropagatesDefaultStatus(t *testing.T) {
	base, _, w := newDCapPair()
	feErr := errors.New("flush refused")
	base.feErr = feErr
	if err := http.NewResponseController(w).Flush(); err != feErr {
		t.Fatalf("ResponseController.Flush = %v, want the base error by identity", err)
	}
}

type dFeOnlyBase struct {
	http.ResponseWriter
	feErr error
}

func (b *dFeOnlyBase) FlushError() error { return b.feErr }

func TestFlushErrorOnlyBaseRoutesThroughDefault(t *testing.T) {
	feErr := errors.New("flush refused")
	base := &dFeOnlyBase{ResponseWriter: httptest.NewRecorder(), feErr: feErr}
	dw := &defaultStatusWriter{ResponseWriter: base, code: http.StatusNotFound}
	w := wrapDefaultStatusWriter(dw)

	if _, ok := w.(http.Flusher); !ok {
		t.Fatal("FlushError-only base must still advertise flushing through the wrapper")
	}
	if err := http.NewResponseController(w).Flush(); err != feErr {
		t.Fatalf("RC.Flush = %v, want the base error by identity", err)
	}
}

func TestFlushErrorThroughNestedDefaultStatusWriters(t *testing.T) {
	feErr := errors.New("flush refused")
	base := &dFeOnlyBase{ResponseWriter: httptest.NewRecorder(), feErr: feErr}
	dw1 := &defaultStatusWriter{ResponseWriter: base, code: http.StatusNotFound}
	inner := wrapDefaultStatusWriter(dw1)
	dw2 := &defaultStatusWriter{ResponseWriter: inner, code: http.StatusNotFound}
	outer := wrapDefaultStatusWriter(dw2)

	if err := http.NewResponseController(outer).Flush(); err != feErr {
		t.Fatalf("two-hop RC.Flush = %v, want the base error by identity", err)
	}
}

type hijackErrRecorder struct {
	*httptest.ResponseRecorder
}

func (r *hijackErrRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, _ := net.Pipe()
	rw := bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c))
	return c, rw, nil
}

func (r *hijackErrRecorder) ReadFrom(src io.Reader) (int64, error) {
	return io.Copy(r.ResponseRecorder, src)
}

func TestNotFoundHandlerPreservesCapabilities(t *testing.T) {
	var sawHijacker, sawReaderFrom bool
	r := New()
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		_, sawHijacker = w.(http.Hijacker)
		_, sawReaderFrom = w.(io.ReaderFrom)
	})

	rec := &hijackErrRecorder{ResponseRecorder: httptest.NewRecorder()}
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/missing", nil))

	if !sawHijacker {
		t.Error("custom 404 handler stripped Hijacker from the underlying writer")
	}
	if !sawReaderFrom {
		t.Error("custom 404 handler stripped ReaderFrom from the underlying writer")
	}
}

type recordingFlusher struct {
	http.ResponseWriter
	events []string
}

func (r *recordingFlusher) WriteHeader(code int) {
	r.events = append(r.events, fmt.Sprintf("header:%d", code))
	r.ResponseWriter.WriteHeader(code)
}

func (r *recordingFlusher) Flush() { r.events = append(r.events, "flush") }

// Flush must send the pending 404 or 405 before it can implicitly commit 200.
func TestFlushWritesPendingDefaultStatus(t *testing.T) {
	base := &recordingFlusher{ResponseWriter: httptest.NewRecorder()}
	dw := &defaultStatusWriter{ResponseWriter: base, code: http.StatusNotFound}
	w := wrapDefaultStatusWriter(dw)

	w.(http.Flusher).Flush()

	want := []string{"header:404", "flush"}
	if len(base.events) != 2 || base.events[0] != want[0] || base.events[1] != want[1] {
		t.Fatalf("events = %v, want %v", base.events, want)
	}
}
