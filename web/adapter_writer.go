package web

import (
	"bufio"
	"io"
	"net"
	"net/http"
)

// Unchecked assertions are safe because wrappers expose only capabilities supported by the underlying writer.
func (c *commitWriter) flush() {
	c.committed = true
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
		return
	}
	c.ResponseWriter.(interface{ FlushError() error }).FlushError() //nolint:errcheck // plain Flush has no error channel
}

// Prefer FlushError because http.ResponseController otherwise loses flush errors through http.Flusher.
func (c *commitWriter) flushError() error {
	c.committed = true
	if fe, ok := c.ResponseWriter.(interface{ FlushError() error }); ok {
		return fe.FlushError()
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (c *commitWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := c.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		// A successful hijack commits the response because the connection now belongs to the handler.
		c.committed = true
	}
	return conn, rw, err
}

func (c *commitWriter) push(target string, opts *http.PushOptions) error {
	return c.ResponseWriter.(http.Pusher).Push(target, opts)
}

func (c *commitWriter) readFrom(src io.Reader) (int64, error) {
	c.committed = true
	return c.ResponseWriter.(io.ReaderFrom).ReadFrom(src)
}

// FlushError-only writers must still expose http.Flusher through the wrapper.
func canFlush(w http.ResponseWriter) bool {
	if _, ok := w.(http.Flusher); ok {
		return true
	}
	_, ok := w.(interface{ FlushError() error })
	return ok
}

type flushPart struct{ cw *commitWriter }

func (p flushPart) Flush() { p.cw.flush() }

func (p flushPart) FlushError() error { return p.cw.flushError() }

type hijackPart struct{ cw *commitWriter }

func (p hijackPart) Hijack() (net.Conn, *bufio.ReadWriter, error) { return p.cw.hijack() }

type pushPart struct{ cw *commitWriter }

func (p pushPart) Push(target string, opts *http.PushOptions) error { return p.cw.push(target, opts) }

type readFromPart struct{ cw *commitWriter }

func (p readFromPart) ReadFrom(src io.Reader) (int64, error) { return p.cw.readFrom(src) }

// Separate wrapper types prevent unsupported optional methods from leaking.
type (
	wF struct {
		*commitWriter
		flushPart
	}
	wH struct {
		*commitWriter
		hijackPart
	}
	wP struct {
		*commitWriter
		pushPart
	}
	wR struct {
		*commitWriter
		readFromPart
	}
	wFH struct {
		*commitWriter
		flushPart
		hijackPart
	}
	wFP struct {
		*commitWriter
		flushPart
		pushPart
	}
	wFR struct {
		*commitWriter
		flushPart
		readFromPart
	}
	wHP struct {
		*commitWriter
		hijackPart
		pushPart
	}
	wHR struct {
		*commitWriter
		hijackPart
		readFromPart
	}
	wPR struct {
		*commitWriter
		pushPart
		readFromPart
	}
	wFHP struct {
		*commitWriter
		flushPart
		hijackPart
		pushPart
	}
	wFHR struct {
		*commitWriter
		flushPart
		hijackPart
		readFromPart
	}
	wFPR struct {
		*commitWriter
		flushPart
		pushPart
		readFromPart
	}
	wHPR struct {
		*commitWriter
		hijackPart
		pushPart
		readFromPart
	}
	wFHPR struct {
		*commitWriter
		flushPart
		hijackPart
		pushPart
		readFromPart
	}
)

func wrapCommitWriter(cw *commitWriter) http.ResponseWriter {
	mask := 0
	if canFlush(cw.ResponseWriter) {
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
