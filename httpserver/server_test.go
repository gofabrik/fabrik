package httpserver

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	for range 200 {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never started listening")
}

func TestServer_GracefulShutdownDrainsInflight(t *testing.T) {
	addr := freeAddr(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var completed bool
	s := New(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
		completed = true
	}), &http.Server{Addr: addr})

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()
	waitListening(t, addr)

	respErr := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err == nil {
			resp.Body.Close()
		}
		respErr <- err
	}()

	<-started
	cancel()
	select {
	case err := <-runErr:
		t.Fatalf("Run returned before the in-flight request finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run = %v, want nil on graceful shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the request completed")
	}
	if !completed {
		t.Fatal("graceful shutdown must let the in-flight request finish, not abort it")
	}
	if err := <-respErr; err != nil {
		t.Fatalf("the in-flight request should complete during graceful shutdown: %v", err)
	}
}

func TestServer_ExternalShutdownNormalizesToNil(t *testing.T) {
	addr := freeAddr(t)
	srv := &http.Server{Addr: addr}
	s := New(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), srv)

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(context.Background()) }()
	waitListening(t, addr)

	// Run treats ErrServerClosed from external shutdown as success.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run = %v, want nil after external shutdown (ErrServerClosed normalized)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the server was shut down")
	}
}

func TestServer_DefaultsToPort8080WhenServerNil(t *testing.T) {
	if got := New(nil, nil).httpServer().Addr; got != ":8080" {
		t.Errorf("nil *http.Server should default to :8080, got %q", got)
	}
	provided := &http.Server{Addr: ":9000"}
	if got := New(nil, provided).httpServer(); got != provided {
		t.Errorf("a provided *http.Server must be used verbatim, got %v", got)
	}
}

func TestServer_ReturnsListenError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := New(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &http.Server{Addr: l.Addr().String()})
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("Run must return the listen error when the address is already in use")
	}
}
