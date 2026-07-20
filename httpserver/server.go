// Package httpserver serves HTTP handlers with graceful shutdown.
package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Server serves a handler with graceful shutdown.
type Server struct {
	Handler http.Handler
	Server  *http.Server // A nil value listens on :8080.
}

// New returns a Server for handler, optionally customized by srv.
func New(handler http.Handler, srv *http.Server) *Server {
	return &Server{Handler: handler, Server: srv}
}

func (s *Server) httpServer() *http.Server {
	if s.Server != nil {
		return s.Server
	}
	return &http.Server{
		Addr:              ":8080",
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// Run returns listener errors, treats [http.ErrServerClosed] as success, and allows 30 seconds for shutdown after cancellation.
func (s *Server) Run(ctx context.Context) error {
	srv := s.httpServer()
	srv.Handler = s.Handler
	errc := make(chan error, 1)
	go func() {
		if srv.TLSConfig != nil {
			errc <- srv.ListenAndServeTLS("", "")
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
