package router

import (
	"bufio"
	"io"
	"net"
	"net/http"
)

// Unchecked assertions are safe because wrappers expose only capabilities supported by the underlying writer.
func (w *defaultStatusWriter) flush() {
	// Send the pending status before Flush can implicitly commit 200.
	if !w.wrote {
		w.WriteHeader(w.code)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
		return
	}
	w.ResponseWriter.(interface{ FlushError() error }).FlushError() //nolint:errcheck // plain Flush has no error channel
}

// Prefer FlushError because http.ResponseController otherwise loses flush errors through http.Flusher.
func (w *defaultStatusWriter) flushError() error {
	if !w.wrote {
		w.WriteHeader(w.code)
	}
	if fe, ok := w.ResponseWriter.(interface{ FlushError() error }); ok {
		return fe.FlushError()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (w *defaultStatusWriter) hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	if err == nil {
		// A successful hijack commits the response because the connection now belongs to the handler.
		w.wrote = true
	}
	return conn, rw, err
}

func (w *defaultStatusWriter) push(target string, opts *http.PushOptions) error {
	return w.ResponseWriter.(http.Pusher).Push(target, opts)
}

func (w *defaultStatusWriter) readFrom(src io.Reader) (int64, error) {
	if !w.wrote {
		w.WriteHeader(w.code)
	}
	return w.ResponseWriter.(io.ReaderFrom).ReadFrom(src)
}

// FlushError-only writers must still expose http.Flusher through the wrapper.
func dCanFlush(w http.ResponseWriter) bool {
	if _, ok := w.(http.Flusher); ok {
		return true
	}
	_, ok := w.(interface{ FlushError() error })
	return ok
}

type dwFlushPart struct{ dw *defaultStatusWriter }

func (p dwFlushPart) Flush() { p.dw.flush() }

func (p dwFlushPart) FlushError() error { return p.dw.flushError() }

type dwHijackPart struct{ dw *defaultStatusWriter }

func (p dwHijackPart) Hijack() (net.Conn, *bufio.ReadWriter, error) { return p.dw.hijack() }

type dwPushPart struct{ dw *defaultStatusWriter }

func (p dwPushPart) Push(target string, opts *http.PushOptions) error {
	return p.dw.push(target, opts)
}

type dwReadFromPart struct{ dw *defaultStatusWriter }

func (p dwReadFromPart) ReadFrom(src io.Reader) (int64, error) { return p.dw.readFrom(src) }

// Separate wrapper types prevent unsupported optional methods from leaking.
type (
	dwF struct {
		*defaultStatusWriter
		dwFlushPart
	}
	dwH struct {
		*defaultStatusWriter
		dwHijackPart
	}
	dwP struct {
		*defaultStatusWriter
		dwPushPart
	}
	dwR struct {
		*defaultStatusWriter
		dwReadFromPart
	}
	dwFH struct {
		*defaultStatusWriter
		dwFlushPart
		dwHijackPart
	}
	dwFP struct {
		*defaultStatusWriter
		dwFlushPart
		dwPushPart
	}
	dwFR struct {
		*defaultStatusWriter
		dwFlushPart
		dwReadFromPart
	}
	dwHP struct {
		*defaultStatusWriter
		dwHijackPart
		dwPushPart
	}
	dwHR struct {
		*defaultStatusWriter
		dwHijackPart
		dwReadFromPart
	}
	dwPR struct {
		*defaultStatusWriter
		dwPushPart
		dwReadFromPart
	}
	dwFHP struct {
		*defaultStatusWriter
		dwFlushPart
		dwHijackPart
		dwPushPart
	}
	dwFHR struct {
		*defaultStatusWriter
		dwFlushPart
		dwHijackPart
		dwReadFromPart
	}
	dwFPR struct {
		*defaultStatusWriter
		dwFlushPart
		dwPushPart
		dwReadFromPart
	}
	dwHPR struct {
		*defaultStatusWriter
		dwHijackPart
		dwPushPart
		dwReadFromPart
	}
	dwFHPR struct {
		*defaultStatusWriter
		dwFlushPart
		dwHijackPart
		dwPushPart
		dwReadFromPart
	}
)

func wrapDefaultStatusWriter(dw *defaultStatusWriter) http.ResponseWriter {
	mask := 0
	if dCanFlush(dw.ResponseWriter) {
		mask |= 1
	}
	if _, ok := dw.ResponseWriter.(http.Hijacker); ok {
		mask |= 2
	}
	if _, ok := dw.ResponseWriter.(http.Pusher); ok {
		mask |= 4
	}
	if _, ok := dw.ResponseWriter.(io.ReaderFrom); ok {
		mask |= 8
	}
	f, h, p, r := dwFlushPart{dw}, dwHijackPart{dw}, dwPushPart{dw}, dwReadFromPart{dw}
	switch mask {
	case 1:
		return &dwF{dw, f}
	case 2:
		return &dwH{dw, h}
	case 3:
		return &dwFH{dw, f, h}
	case 4:
		return &dwP{dw, p}
	case 5:
		return &dwFP{dw, f, p}
	case 6:
		return &dwHP{dw, h, p}
	case 7:
		return &dwFHP{dw, f, h, p}
	case 8:
		return &dwR{dw, r}
	case 9:
		return &dwFR{dw, f, r}
	case 10:
		return &dwHR{dw, h, r}
	case 11:
		return &dwFHR{dw, f, h, r}
	case 12:
		return &dwPR{dw, p, r}
	case 13:
		return &dwFPR{dw, f, p, r}
	case 14:
		return &dwHPR{dw, h, p, r}
	case 15:
		return &dwFHPR{dw, f, h, p, r}
	default:
		return dw
	}
}
