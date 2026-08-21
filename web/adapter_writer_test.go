package web

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type capBase struct {
	http.ResponseWriter
	// Capability methods record whether commit occurs before delegation.
	cw              *commitWriter
	committedAtCall []bool
	flushed         int
	hijackErr       error
	hijackConn      net.Conn
	hijackRW        *bufio.ReadWriter
	pushed          []string
	pushOpts        []*http.PushOptions
	pushErr         error
	readFrom        bytes.Buffer
	rfSrc           io.Reader
	feErr           error
	rfN             int64
	rfErr           error
}

func (b *capBase) record() { b.committedAtCall = append(b.committedAtCall, b.cw.committed) }

func (b *capBase) Flush() { b.record(); b.flushed++ }

func (b *capBase) FlushError() error { b.record(); b.flushed++; return b.feErr }

func (b *capBase) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	b.record()
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
	b.record()
	b.pushed = append(b.pushed, target)
	b.pushOpts = append(b.pushOpts, opts)
	return b.pushErr
}

func (b *capBase) ReadFrom(src io.Reader) (int64, error) {
	b.record()
	b.rfSrc = src
	if b.rfErr != nil {
		return b.rfN, b.rfErr
	}
	return io.Copy(&b.readFrom, src)
}

func newCapPair() (*capBase, *commitWriter, http.ResponseWriter) {
	base := &capBase{ResponseWriter: httptest.NewRecorder()}
	cw := &commitWriter{ResponseWriter: base}
	base.cw = cw
	return base, cw, wrapCommitWriter(cw)
}

func TestCapabilityBehaviorForwarding(t *testing.T) {
	base, cw, w := newCapPair()

	w.(http.Flusher).Flush()
	if base.flushed != 1 {
		t.Fatalf("flush not forwarded, count=%d", base.flushed)
	}
	if !cw.committed {
		t.Fatal("flush must mark the writer committed")
	}
	if len(base.committedAtCall) != 1 || !base.committedAtCall[0] {
		t.Fatal("flush must commit BEFORE delegating, not after")
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

	base2, cw2, w2 := newCapPair()
	src := strings.NewReader("payload")
	n, err := w2.(io.ReaderFrom).ReadFrom(src)
	if err != nil || n != int64(len("payload")) || base2.readFrom.String() != "payload" {
		t.Fatalf("readFrom not forwarded: n=%d err=%v got=%q", n, err, base2.readFrom.String())
	}
	if base2.rfSrc != src {
		t.Fatal("readFrom source not forwarded by identity")
	}
	if !cw2.committed {
		t.Fatal("readFrom must mark the writer committed")
	}
	if len(base2.committedAtCall) != 1 || !base2.committedAtCall[0] {
		t.Fatal("readFrom must commit BEFORE delegating, not after")
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

func TestHijackMarksCommitted(t *testing.T) {
	_, cw, w := newCapPair()

	if cw.committed {
		t.Fatal("committed before any capability call")
	}
	if _, _, err := w.(http.Hijacker).Hijack(); err != nil {
		t.Fatal(err)
	}
	if !cw.committed {
		t.Fatal("successful hijack must mark the writer committed")
	}
}

func TestFailedHijackDoesNotCommit(t *testing.T) {
	base, cw, w := newCapPair()
	base.hijackErr = errors.New("no hijack")

	if _, _, err := w.(http.Hijacker).Hijack(); err != base.hijackErr {
		t.Fatalf("hijack error not forwarded by identity: %v", err)
	}
	if cw.committed {
		t.Fatal("failed hijack must not mark the writer committed")
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
	// Every wrapper retains the Unwrap chain for ResponseController.
	u, ok := w.(interface{ Unwrap() http.ResponseWriter })
	if !ok || u.Unwrap() != base {
		t.Errorf("%s: Unwrap chain broken", name)
	}
}

func TestWrapCommitWriterSelection(t *testing.T) {
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
		got := wrapCommitWriter(&commitWriter{ResponseWriter: tc.base})
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
func TestFlushErrorPropagates(t *testing.T) {
	base, cw, w := newCapPair()
	feErr := errors.New("flush refused")
	base.feErr = feErr
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != feErr {
		t.Fatalf("ResponseController.Flush = %v, want the base error by identity", err)
	}
	if !cw.committed {
		t.Fatal("FlushError must mark the writer committed")
	}
	if len(base.committedAtCall) != 1 || !base.committedAtCall[0] {
		t.Fatal("FlushError must commit BEFORE delegating")
	}
}

type feOnlyBase struct {
	http.ResponseWriter
	cw     *commitWriter
	atCall []bool
	feErr  error
}

func (b *feOnlyBase) FlushError() error { b.atCall = append(b.atCall, b.cw.committed); return b.feErr }

func TestFlushErrorOnlyBaseRoutesThroughCommit(t *testing.T) {
	base := &feOnlyBase{ResponseWriter: httptest.NewRecorder(), feErr: errors.New("flush refused")}
	cw := &commitWriter{ResponseWriter: base}
	base.cw = cw
	w := wrapCommitWriter(cw)

	if _, ok := w.(http.Flusher); !ok {
		t.Fatal("FlushError-only base must still advertise flushing through the wrapper")
	}
	if err := http.NewResponseController(w).Flush(); err != base.feErr {
		t.Fatalf("RC.Flush = %v, want the base error by identity", err)
	}
	if len(base.atCall) != 1 || !base.atCall[0] {
		t.Fatal("FlushError-only path must commit BEFORE delegating")
	}
}

func TestFlushErrorThroughNestedWrappers(t *testing.T) {
	base := &feOnlyBase{ResponseWriter: httptest.NewRecorder(), feErr: errors.New("flush refused")}
	inner := &commitWriter{ResponseWriter: base}
	base.cw = inner
	outer := &commitWriter{ResponseWriter: wrapCommitWriter(inner)}
	w := wrapCommitWriter(outer)

	if err := http.NewResponseController(w).Flush(); err != base.feErr {
		t.Fatalf("two-hop RC.Flush = %v, want the base error by identity", err)
	}
	if !outer.committed || !inner.committed {
		t.Fatal("both wrappers must commit on the flush path")
	}
}
