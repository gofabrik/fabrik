package middleware

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/cli"
)

// stubCtx isolates middleware tests from parsing.
type stubCtx struct {
	context.Context
	out, errW *bytes.Buffer
	path      []string
}

func (s *stubCtx) Stdout() io.Writer     { return s.out }
func (s *stubCtx) Stderr() io.Writer     { return s.errW }
func (s *stubCtx) Stdin() io.Reader      { return bytes.NewReader(nil) }
func (s *stubCtx) CommandPath() []string { return s.path }

func newStub() *stubCtx {
	return &stubCtx{
		Context: context.Background(),
		out:     &bytes.Buffer{},
		errW:    &bytes.Buffer{},
		path:    []string{"app"},
	}
}

var _ cli.Context = (*stubCtx)(nil)

func TestRecover_TurnsPanicIntoError(t *testing.T) {
	called := false
	h := Recover()(cli.Handler(func(cli.Context) error {
		called = true
		panic("boom")
	}))
	err := h(newStub())
	if !called {
		t.Fatal("handler never ran")
	}
	if err == nil || !strings.Contains(err.Error(), "panic: boom") {
		t.Errorf("want panic-converted error, got %v", err)
	}
}

func TestTimeout_CancelsHandler(t *testing.T) {
	h := Timeout(20 * time.Millisecond)(cli.Handler(func(ctx cli.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
			return errors.New("timeout did not fire")
		}
	}))
	err := h(newStub())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want DeadlineExceeded, got %v", err)
	}
}

func TestTimeout_ZeroIsPassthrough(t *testing.T) {
	called := false
	h := Timeout(0)(cli.Handler(func(cli.Context) error {
		called = true
		return nil
	}))
	if err := h(newStub()); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("handler should run when timeout is 0")
	}
}

func TestLogging_WritesSummaryAfterHandler(t *testing.T) {
	stub := newStub()
	stub.path = []string{"myapp", "serve"}
	h := Logging()(cli.Handler(func(cli.Context) error { return nil }))
	if err := h(stub); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stub.errW.String(), "myapp serve took") {
		t.Errorf("want command path + took line, got %q", stub.errW.String())
	}
}
