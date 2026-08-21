package web_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/web"
)

type capVisibility struct {
	hijacker   *bool
	flusher    *bool
	readerFrom *bool
	rcUnwrap   *bool
}

func (r capVisibility) Respond(w http.ResponseWriter, _ *http.Request) error {
	_, *r.hijacker = w.(http.Hijacker)
	_, *r.flusher = w.(http.Flusher)
	_, *r.readerFrom = w.(io.ReaderFrom)
	rc := http.NewResponseController(w)
	// SetWriteDeadline succeeds only if Unwrap lets ResponseController reach the underlying writer.
	*r.rcUnwrap = rc.SetWriteDeadline(time.Now().Add(time.Second)) == nil
	return nil
}

func TestWrapPreservesCapabilities(t *testing.T) {
	var hijacker, flusher, readerFrom, rcUnwrap bool
	a := web.NewAdapter()
	h := a.Wrap(func(req *web.Request) (web.Response, error) {
		return capVisibility{
			hijacker:   &hijacker,
			flusher:    &flusher,
			readerFrom: &readerFrom,
			rcUnwrap:   &rcUnwrap,
		}, nil
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL) //nolint:noctx
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if !hijacker {
		t.Error("Wrap must preserve http.Hijacker on a real HTTP/1.1 connection")
	}
	if !flusher {
		t.Error("Wrap must preserve http.Flusher on a real HTTP/1.1 connection")
	}
	if !readerFrom {
		t.Error("Wrap must preserve io.ReaderFrom on a real HTTP/1.1 connection")
	}
	if !rcUnwrap {
		t.Error("http.ResponseController must reach the underlying writer via Unwrap")
	}
}

// The connection stays open after the handler error so any fallback response remains observable.
type hijackErrorResponse struct {
	wrote chan<- string
	conns chan<- net.Conn
}

func (r hijackErrorResponse) Respond(w http.ResponseWriter, _ *http.Request) error {
	h, ok := w.(http.Hijacker)
	if !ok {
		r.wrote <- ""
		w.WriteHeader(http.StatusOK)
		return nil
	}
	conn, rw, err := h.Hijack()
	if err != nil {
		r.wrote <- ""
		return err
	}
	marker := "HIJACKED\n"
	rw.WriteString(marker) //nolint:errcheck
	rw.Flush()             //nolint:errcheck
	r.conns <- conn
	r.wrote <- marker
	return errors.New("error after hijack")
}

func TestWrapHijackThenErrorDoesNotWriteToConnection(t *testing.T) {
	wrote := make(chan string, 1)
	conns := make(chan net.Conn, 1)
	var errHandlerCalled atomic.Bool
	a := web.NewAdapter(web.WithErrorHandler(func(w http.ResponseWriter, r *http.Request, err error) {
		errHandlerCalled.Store(true)
		w.Write([]byte("ERROR-BODY")) //nolint:errcheck
	}))
	h := a.Wrap(func(req *web.Request) (web.Response, error) {
		return hijackErrorResponse{wrote: wrote, conns: conns}, nil
	})
	srv := httptest.NewServer(h)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n") //nolint:errcheck

	var handlerWrote string
	select {
	case handlerWrote = <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not signal within timeout")
	}
	if handlerWrote == "" {
		t.Fatal("http.Hijacker not available through Wrap")
	}
	hijacked := <-conns
	defer hijacked.Close()

	// Read to the deadline to capture any fallback bytes written after hijacking.
	var buf bytes.Buffer
	conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
	io.Copy(&buf, conn)                                          //nolint:errcheck

	// Closing the server waits for the handler before errHandlerCalled is read.
	srv.Close()

	if errHandlerCalled.Load() {
		t.Error("error handler must not run after a successful hijack")
	}
	if got := buf.String(); got != handlerWrote {
		t.Errorf("connection received %q, want exactly %q", got, handlerWrote)
	}
}
