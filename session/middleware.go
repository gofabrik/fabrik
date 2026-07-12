package session

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
)

// Middleware attaches request session state and commits it before
// response headers leave.
//
// Store access is lazy; untouched requests do no session I/O.
//
// Middleware can be passed as a method value or used to wrap a handler:
//
//	mux.Handle("/", sessMgr.Middleware(app))
func (m *core) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid, _ := m.cfg.Token.Read(r)
		st := &state{arrivedSID: sid}
		ctx := context.WithValue(r.Context(), m.ctxKey, st)

		// Commit after handler side effects even if the client
		// disconnects.
		commitCtx := context.WithoutCancel(ctx)
		cw := &committingWriter{
			ResponseWriter: w,
			commitFn: func(rw http.ResponseWriter) error {
				return m.commit(commitCtx, st, rw)
			},
		}
		next.ServeHTTP(wrapWriter(cw), r.WithContext(ctx))

		// The implicit 200 can still carry Set-Cookie.
		if !cw.headerWritten {
			cw.WriteHeader(http.StatusOK)
		}
	})
}

// committingWriter commits before response headers leave. A commit
// failure replaces the response with a 500.
type committingWriter struct {
	http.ResponseWriter
	commitFn      func(http.ResponseWriter) error
	headerWritten bool
	committed     bool
	failed        bool
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (cw *committingWriter) Unwrap() http.ResponseWriter { return cw.ResponseWriter }

func (cw *committingWriter) WriteHeader(status int) {
	if cw.headerWritten {
		return
	}
	cw.runCommit()
	if cw.headerWritten {
		return
	}
	cw.headerWritten = true
	cw.ResponseWriter.WriteHeader(status)
}

func (cw *committingWriter) Write(b []byte) (int, error) {
	if !cw.headerWritten {
		cw.runCommit()
		cw.headerWritten = true
	}
	if cw.failed {
		return len(b), nil
	}
	return cw.ResponseWriter.Write(b)
}

func (cw *committingWriter) runCommit() {
	if cw.committed {
		return
	}
	cw.committed = true
	if err := cw.commitFn(cw.ResponseWriter); err != nil {
		http.Error(cw.ResponseWriter, "session commit failed: "+err.Error(), http.StatusInternalServerError)
		cw.headerWritten = true
		cw.failed = true
	}
}

// flush commits before flushing through.
func (cw *committingWriter) flush() {
	if !cw.headerWritten {
		cw.runCommit()
		cw.headerWritten = true
	}
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// hijack commits before the connection is taken over. Tokens staged
// in headers are not serialized after hijack.
func (cw *committingWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := cw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	cw.runCommit()
	if cw.failed {
		return nil, nil, http.ErrHijacked
	}
	conn, brw, err := h.Hijack()
	if err == nil {
		cw.headerWritten = true
	}
	return conn, brw, err
}

// readFrom commits before optimized body copies.
func (cw *committingWriter) readFrom(src io.Reader) (int64, error) {
	if !cw.headerWritten {
		cw.runCommit()
		cw.headerWritten = true
	}
	if cw.failed {
		return io.Copy(io.Discard, src)
	}
	if rf, ok := cw.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(struct{ io.Writer }{cw.ResponseWriter}, src)
}

// push delegates HTTP/2 push through the underlying writer.
func (cw *committingWriter) push(target string, opts *http.PushOptions) error {
	if p, ok := cw.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

type flushPart struct{ cw *committingWriter }

func (p flushPart) Flush() { p.cw.flush() }

type hijackPart struct{ cw *committingWriter }

func (p hijackPart) Hijack() (net.Conn, *bufio.ReadWriter, error) { return p.cw.hijack() }

type pushPart struct{ cw *committingWriter }

func (p pushPart) Push(target string, opts *http.PushOptions) error { return p.cw.push(target, opts) }

type readFromPart struct{ cw *committingWriter }

func (p readFromPart) ReadFrom(src io.Reader) (int64, error) { return p.cw.readFrom(src) }

// Each wrapper advertises exactly the optional interfaces supported by
// the underlying writer.
type (
	wF struct {
		*committingWriter
		flushPart
	}
	wH struct {
		*committingWriter
		hijackPart
	}
	wP struct {
		*committingWriter
		pushPart
	}
	wR struct {
		*committingWriter
		readFromPart
	}
	wFH struct {
		*committingWriter
		flushPart
		hijackPart
	}
	wFP struct {
		*committingWriter
		flushPart
		pushPart
	}
	wFR struct {
		*committingWriter
		flushPart
		readFromPart
	}
	wHP struct {
		*committingWriter
		hijackPart
		pushPart
	}
	wHR struct {
		*committingWriter
		hijackPart
		readFromPart
	}
	wPR struct {
		*committingWriter
		pushPart
		readFromPart
	}
	wFHP struct {
		*committingWriter
		flushPart
		hijackPart
		pushPart
	}
	wFHR struct {
		*committingWriter
		flushPart
		hijackPart
		readFromPart
	}
	wFPR struct {
		*committingWriter
		flushPart
		pushPart
		readFromPart
	}
	wHPR struct {
		*committingWriter
		hijackPart
		pushPart
		readFromPart
	}
	wFHPR struct {
		*committingWriter
		flushPart
		hijackPart
		pushPart
		readFromPart
	}
)

// wrapWriter preserves the underlying writer's optional interfaces.
func wrapWriter(cw *committingWriter) http.ResponseWriter {
	mask := 0
	if _, ok := cw.ResponseWriter.(http.Flusher); ok {
		mask |= 1
	}
	if _, ok := cw.ResponseWriter.(http.Hijacker); ok {
		mask |= 2
	}
	if _, ok := cw.ResponseWriter.(http.Pusher); ok {
		mask |= 4
	}
	if _, ok := cw.ResponseWriter.(io.ReaderFrom); ok {
		mask |= 8
	}
	f, h, p, r := flushPart{cw}, hijackPart{cw}, pushPart{cw}, readFromPart{cw}
	switch mask {
	case 1:
		return &wF{cw, f}
	case 2:
		return &wH{cw, h}
	case 3:
		return &wFH{cw, f, h}
	case 4:
		return &wP{cw, p}
	case 5:
		return &wFP{cw, f, p}
	case 6:
		return &wHP{cw, h, p}
	case 7:
		return &wFHP{cw, f, h, p}
	case 8:
		return &wR{cw, r}
	case 9:
		return &wFR{cw, f, r}
	case 10:
		return &wHR{cw, h, r}
	case 11:
		return &wFHR{cw, f, h, r}
	case 12:
		return &wPR{cw, p, r}
	case 13:
		return &wFPR{cw, f, p, r}
	case 14:
		return &wHPR{cw, h, p, r}
	case 15:
		return &wFHPR{cw, f, h, p, r}
	default:
		return cw
	}
}
