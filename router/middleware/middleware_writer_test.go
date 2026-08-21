package middleware

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

type capBase struct {
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

func (b *capBase) Flush() { b.flushed++ }

func (b *capBase) FlushError() error { b.flushed++; return b.feErr }

func (b *capBase) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if b.hijackErr != nil {
		return nil, nil, b.hijackErr
	}
	if b.hijackConn == nil {
		b.hijackConn, _ = net.Pipe()
		b.hijackRW = bufio.NewReadWriter(bufio.NewReader(b.hijackConn), bufio.NewWriter(b.hijackConn))
	}
	return b.hijackConn, b.hijackRW, nil
}

func (b *capBase) Push(target string, opts *http.PushOptions) error {
	b.pushed = append(b.pushed, target)
	b.pushOpts = append(b.pushOpts, opts)
	return b.pushErr
}

func (b *capBase) ReadFrom(src io.Reader) (int64, error) {
	b.rfSrc = src
	if b.rfErr != nil {
		return b.rfN, b.rfErr
	}
	return io.Copy(&b.readFrom, src)
}

func newCapPair() (*capBase, *statusWriter, http.ResponseWriter) {
	base := &capBase{ResponseWriter: httptest.NewRecorder()}
	sw := &statusWriter{ResponseWriter: base, status: http.StatusOK}
	return base, sw, wrapStatusWriter(sw)
}

func TestStatusWriterCapabilityBehaviorForwarding(t *testing.T) {
	base, _, w := newCapPair()

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

	base2, _, w2 := newCapPair()
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

func TestFailedHijackStatusWriter(t *testing.T) {
	base, _, w := newCapPair()
	base.hijackErr = errors.New("no hijack")

	if _, _, err := w.(http.Hijacker).Hijack(); err != base.hijackErr {
		t.Fatalf("hijack error not forwarded by identity: %v", err)
	}
}

type baseWriter struct{}

func (baseWriter) Header() http.Header       { return nil }
func (baseWriter) Write([]byte) (int, error) { return 0, nil }
func (baseWriter) WriteHeader(int)           {}

type mF struct{}

func (mF) Flush() {}

type mH struct{}

func (mH) Hijack() (net.Conn, *bufio.ReadWriter, error) { return nil, nil, nil }

type mP struct{}

func (mP) Push(string, *http.PushOptions) error { return nil }

type mR struct{}

func (mR) ReadFrom(io.Reader) (int64, error) { return 0, nil }

type (
	bF struct {
		baseWriter
		mF
	}
	bH struct {
		baseWriter
		mH
	}
	bP struct {
		baseWriter
		mP
	}
	bR struct {
		baseWriter
		mR
	}
	bFH struct {
		baseWriter
		mF
		mH
	}
	bFP struct {
		baseWriter
		mF
		mP
	}
	bFR struct {
		baseWriter
		mF
		mR
	}
	bHP struct {
		baseWriter
		mH
		mP
	}
	bHR struct {
		baseWriter
		mH
		mR
	}
	bPR struct {
		baseWriter
		mP
		mR
	}
	bFHP struct {
		baseWriter
		mF
		mH
		mP
	}
	bFHR struct {
		baseWriter
		mF
		mH
		mR
	}
	bFPR struct {
		baseWriter
		mF
		mP
		mR
	}
	bHPR struct {
		baseWriter
		mH
		mP
		mR
	}
	bFHPR struct {
		baseWriter
		mF
		mH
		mP
		mR
	}
)

type wantCaps struct{ f, h, p, r bool }

func assertCaps(t *testing.T, name string, w http.ResponseWriter, want wantCaps, base http.ResponseWriter) {
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

func TestWrapStatusWriterSelection(t *testing.T) {
	cases := []struct {
		name string
		base http.ResponseWriter
		want wantCaps
	}{
		{"none", baseWriter{}, wantCaps{}},
		{"F", bF{}, wantCaps{f: true}},
		{"H", bH{}, wantCaps{h: true}},
		{"P", bP{}, wantCaps{p: true}},
		{"R", bR{}, wantCaps{r: true}},
		{"FH", bFH{}, wantCaps{f: true, h: true}},
		{"FP", bFP{}, wantCaps{f: true, p: true}},
		{"FR", bFR{}, wantCaps{f: true, r: true}},
		{"HP", bHP{}, wantCaps{h: true, p: true}},
		{"HR", bHR{}, wantCaps{h: true, r: true}},
		{"PR", bPR{}, wantCaps{p: true, r: true}},
		{"FHP", bFHP{}, wantCaps{f: true, h: true, p: true}},
		{"FHR", bFHR{}, wantCaps{f: true, h: true, r: true}},
		{"FPR", bFPR{}, wantCaps{f: true, p: true, r: true}},
		{"HPR", bHPR{}, wantCaps{h: true, p: true, r: true}},
		{"FHPR", bFHPR{}, wantCaps{f: true, h: true, p: true, r: true}},
	}
	for _, tc := range cases {
		sw := &statusWriter{ResponseWriter: tc.base, status: http.StatusOK}
		got := wrapStatusWriter(sw)
		assertCaps(t, tc.name, got, tc.want, tc.base)
	}
}

// These assertions cover every optional-interface combination, including Pusher combinations unavailable over HTTP/1.
var (
	_ http.Flusher = (*wF)(nil)
	_ http.Flusher = (*wFH)(nil)
	_ http.Flusher = (*wFP)(nil)
	_ http.Flusher = (*wFR)(nil)
	_ http.Flusher = (*wFHP)(nil)
	_ http.Flusher = (*wFHR)(nil)
	_ http.Flusher = (*wFPR)(nil)
	_ http.Flusher = (*wFHPR)(nil)

	_ http.Hijacker = (*wH)(nil)
	_ http.Hijacker = (*wFH)(nil)
	_ http.Hijacker = (*wHP)(nil)
	_ http.Hijacker = (*wHR)(nil)
	_ http.Hijacker = (*wFHP)(nil)
	_ http.Hijacker = (*wFHR)(nil)
	_ http.Hijacker = (*wHPR)(nil)
	_ http.Hijacker = (*wFHPR)(nil)

	_ http.Pusher = (*wP)(nil)
	_ http.Pusher = (*wFP)(nil)
	_ http.Pusher = (*wHP)(nil)
	_ http.Pusher = (*wPR)(nil)
	_ http.Pusher = (*wFHP)(nil)
	_ http.Pusher = (*wFPR)(nil)
	_ http.Pusher = (*wHPR)(nil)
	_ http.Pusher = (*wFHPR)(nil)

	_ io.ReaderFrom = (*wR)(nil)
	_ io.ReaderFrom = (*wFR)(nil)
	_ io.ReaderFrom = (*wHR)(nil)
	_ io.ReaderFrom = (*wPR)(nil)
	_ io.ReaderFrom = (*wFHR)(nil)
	_ io.ReaderFrom = (*wFPR)(nil)
	_ io.ReaderFrom = (*wHPR)(nil)
	_ io.ReaderFrom = (*wFHPR)(nil)
)

// ResponseController prefers FlushError, so wrappers must preserve its error.
func TestFlushErrorPropagatesStatus(t *testing.T) {
	base, _, w := newCapPair()
	feErr := errors.New("flush refused")
	base.feErr = feErr
	if err := http.NewResponseController(w).Flush(); err != feErr {
		t.Fatalf("ResponseController.Flush = %v, want the base error by identity", err)
	}
}

type feOnlyBase struct {
	http.ResponseWriter
	feErr error
}

func (b *feOnlyBase) FlushError() error { return b.feErr }

func TestFlushErrorOnlyBaseRoutesThroughStatus(t *testing.T) {
	feErr := errors.New("flush refused")
	base := &feOnlyBase{ResponseWriter: httptest.NewRecorder(), feErr: feErr}
	sw := &statusWriter{ResponseWriter: base, status: http.StatusOK}
	w := wrapStatusWriter(sw)

	if _, ok := w.(http.Flusher); !ok {
		t.Fatal("FlushError-only base must still advertise flushing through the wrapper")
	}
	if err := http.NewResponseController(w).Flush(); err != feErr {
		t.Fatalf("RC.Flush = %v, want the base error by identity", err)
	}
}

func TestFlushErrorThroughNestedStatusWriters(t *testing.T) {
	feErr := errors.New("flush refused")
	base := &feOnlyBase{ResponseWriter: httptest.NewRecorder(), feErr: feErr}
	sw1 := &statusWriter{ResponseWriter: base, status: http.StatusOK}
	inner := wrapStatusWriter(sw1)
	sw2 := &statusWriter{ResponseWriter: inner, status: http.StatusOK}
	outer := wrapStatusWriter(sw2)

	if err := http.NewResponseController(outer).Flush(); err != feErr {
		t.Fatalf("two-hop RC.Flush = %v, want the base error by identity", err)
	}
}

type hijackRecorder struct {
	*httptest.ResponseRecorder
}

func (r *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, _ := net.Pipe()
	rw := bufio.NewReadWriter(bufio.NewReader(c), bufio.NewWriter(c))
	return c, rw, nil
}

func (r *hijackRecorder) ReadFrom(src io.Reader) (int64, error) {
	return io.Copy(r.ResponseRecorder, src)
}

func TestLoggerPreservesCapabilities(t *testing.T) {
	var sawHijacker, sawReaderFrom bool
	h := Logger(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHijacker = w.(http.Hijacker)
		_, sawReaderFrom = w.(io.ReaderFrom)
	}))

	rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if !sawHijacker {
		t.Error("Logger stripped Hijacker from the underlying writer")
	}
	if !sawReaderFrom {
		t.Error("Logger stripped ReaderFrom from the underlying writer")
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

// Flush commits the current status, which a later WriteHeader cannot change.
func TestFlushFreezesLoggedStatus(t *testing.T) {
	base := &recordingFlusher{ResponseWriter: httptest.NewRecorder()}
	sw := &statusWriter{ResponseWriter: base, status: http.StatusOK}
	w := wrapStatusWriter(sw)

	w.(http.Flusher).Flush()
	w.WriteHeader(http.StatusInternalServerError)

	if sw.status != http.StatusOK {
		t.Fatalf("logged status = %d, want the flushed 200", sw.status)
	}
}
