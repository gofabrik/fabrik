package middleware

import (
	"bufio"
	"io"
	"net"
	"net/http"
)

// Unchecked assertions are safe because wrappers expose only capabilities supported by the underlying writer.
func (w *statusWriter) flush() {
	// Flushing commits the response, so the logged status cannot change afterward.
	w.wrote = true
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
		return
	}
	w.ResponseWriter.(interface{ FlushError() error }).FlushError() //nolint:errcheck // plain Flush has no error channel
}

// Prefer FlushError because http.ResponseController otherwise loses flush errors through http.Flusher.
func (w *statusWriter) flushError() error {
	w.wrote = true
	if fe, ok := w.ResponseWriter.(interface{ FlushError() error }); ok {
		return fe.FlushError()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (w *statusWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		// A successful hijack commits the response because the connection now belongs to the handler.
		w.wrote = true
	}
	return conn, rw, err
}

func (w *statusWriter) push(target string, opts *http.PushOptions) error {
	return w.ResponseWriter.(http.Pusher).Push(target, opts)
}

func (w *statusWriter) readFrom(src io.Reader) (int64, error) {
	w.wrote = true
	return w.ResponseWriter.(io.ReaderFrom).ReadFrom(src)
}

// FlushError-only writers must still expose http.Flusher through the wrapper.
func canFlush(w http.ResponseWriter) bool {
	if _, ok := w.(http.Flusher); ok {
		return true
	}
	_, ok := w.(interface{ FlushError() error })
	return ok
}

type flushPart struct{ sw *statusWriter }

func (p flushPart) Flush() { p.sw.flush() }

func (p flushPart) FlushError() error { return p.sw.flushError() }

type hijackPart struct{ sw *statusWriter }

func (p hijackPart) Hijack() (net.Conn, *bufio.ReadWriter, error) { return p.sw.hijack() }

type pushPart struct{ sw *statusWriter }

func (p pushPart) Push(target string, opts *http.PushOptions) error { return p.sw.push(target, opts) }

type readFromPart struct{ sw *statusWriter }

func (p readFromPart) ReadFrom(src io.Reader) (int64, error) { return p.sw.readFrom(src) }

// Separate wrapper types prevent unsupported optional methods from leaking.
type (
	wF struct {
		*statusWriter
		flushPart
	}
	wH struct {
		*statusWriter
		hijackPart
	}
	wP struct {
		*statusWriter
		pushPart
	}
	wR struct {
		*statusWriter
		readFromPart
	}
	wFH struct {
		*statusWriter
		flushPart
		hijackPart
	}
	wFP struct {
		*statusWriter
		flushPart
		pushPart
	}
	wFR struct {
		*statusWriter
		flushPart
		readFromPart
	}
	wHP struct {
		*statusWriter
		hijackPart
		pushPart
	}
	wHR struct {
		*statusWriter
		hijackPart
		readFromPart
	}
	wPR struct {
		*statusWriter
		pushPart
		readFromPart
	}
	wFHP struct {
		*statusWriter
		flushPart
		hijackPart
		pushPart
	}
	wFHR struct {
		*statusWriter
		flushPart
		hijackPart
		readFromPart
	}
	wFPR struct {
		*statusWriter
		flushPart
		pushPart
		readFromPart
	}
	wHPR struct {
		*statusWriter
		hijackPart
		pushPart
		readFromPart
	}
	wFHPR struct {
		*statusWriter
		flushPart
		hijackPart
		pushPart
		readFromPart
	}
)

func wrapStatusWriter(sw *statusWriter) http.ResponseWriter {
	mask := 0
	if canFlush(sw.ResponseWriter) {
		mask |= 1
	}
	if _, ok := sw.ResponseWriter.(http.Hijacker); ok {
		mask |= 2
	}
	if _, ok := sw.ResponseWriter.(http.Pusher); ok {
		mask |= 4
	}
	if _, ok := sw.ResponseWriter.(io.ReaderFrom); ok {
		mask |= 8
	}
	f, h, p, r := flushPart{sw}, hijackPart{sw}, pushPart{sw}, readFromPart{sw}
	switch mask {
	case 1:
		return &wF{sw, f}
	case 2:
		return &wH{sw, h}
	case 3:
		return &wFH{sw, f, h}
	case 4:
		return &wP{sw, p}
	case 5:
		return &wFP{sw, f, p}
	case 6:
		return &wHP{sw, h, p}
	case 7:
		return &wFHP{sw, f, h, p}
	case 8:
		return &wR{sw, r}
	case 9:
		return &wFR{sw, f, r}
	case 10:
		return &wHR{sw, h, r}
	case 11:
		return &wFHR{sw, f, h, r}
	case 12:
		return &wPR{sw, p, r}
	case 13:
		return &wFPR{sw, f, p, r}
	case 14:
		return &wHPR{sw, h, p, r}
	case 15:
		return &wFHPR{sw, f, h, p, r}
	default:
		return sw
	}
}
