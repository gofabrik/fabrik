// Package bench compares dispatch cost across the stdlib ServeMux, chi, and this
// router over a shared 25-route REST table. Run with:
//
//	GOWORK=off go test -bench . -benchmem ./...
package bench

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	chi "github.com/go-chi/chi/v5"
	"github.com/gofabrik/fabrik/router"
)

var routes = []struct{ method, pattern string }{
	{"GET", "/"},
	{"GET", "/users"},
	{"POST", "/users"},
	{"GET", "/users/{id}"},
	{"PUT", "/users/{id}"},
	{"PATCH", "/users/{id}"},
	{"DELETE", "/users/{id}"},
	{"GET", "/users/{id}/posts"},
	{"POST", "/users/{id}/posts"},
	{"GET", "/users/{id}/posts/{pid}"},
	{"PUT", "/users/{id}/posts/{pid}"},
	{"DELETE", "/users/{id}/posts/{pid}"},
	{"GET", "/users/{id}/posts/{pid}/comments"},
	{"POST", "/users/{id}/posts/{pid}/comments"},
	{"GET", "/orders"},
	{"POST", "/orders"},
	{"GET", "/orders/{id}"},
	{"PUT", "/orders/{id}"},
	{"DELETE", "/orders/{id}"},
	{"GET", "/products"},
	{"GET", "/products/{id}"},
	{"GET", "/search"},
	{"GET", "/health"},
	{"GET", "/metrics"},
	{"GET", "/assets/{path...}"},
}

func noop(http.ResponseWriter, *http.Request) {}

func buildStd() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range routes {
		mux.HandleFunc(rt.method+" "+rt.pattern, noop)
	}
	return mux
}

func buildChi() http.Handler {
	r := chi.NewRouter()
	for _, rt := range routes {
		r.MethodFunc(rt.method, chiPattern(rt.pattern), noop)
	}
	return r
}

// chiPattern rewrites the stdlib wildcard "{path...}" to chi's "*".
func chiPattern(p string) string {
	const w = "{path...}"
	if strings.HasSuffix(p, w) {
		return p[:len(p)-len(w)] + "*"
	}
	return p
}

func buildRouter() http.Handler {
	r := router.New()
	for _, rt := range routes {
		r.Method(rt.method, rt.pattern, noop)
	}
	return r
}

type discard struct{ h http.Header }

func (d *discard) Header() http.Header       { return d.h }
func (*discard) Write(b []byte) (int, error) { return len(b), nil }
func (*discard) WriteHeader(int)             {}

func run(b *testing.B, h http.Handler, method, target string) {
	req := httptest.NewRequest(method, target, nil)
	w := &discard{h: make(http.Header)}
	h.ServeHTTP(w, req)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(w, req)
	}
}

func BenchmarkStaticStd(b *testing.B)    { run(b, buildStd(), "GET", "/health") }
func BenchmarkStaticChi(b *testing.B)    { run(b, buildChi(), "GET", "/health") }
func BenchmarkStaticRouter(b *testing.B) { run(b, buildRouter(), "GET", "/health") }

func BenchmarkParamStd(b *testing.B)    { run(b, buildStd(), "GET", "/users/42") }
func BenchmarkParamChi(b *testing.B)    { run(b, buildChi(), "GET", "/users/42") }
func BenchmarkParamRouter(b *testing.B) { run(b, buildRouter(), "GET", "/users/42") }

func BenchmarkDeepParamStd(b *testing.B) {
	run(b, buildStd(), "GET", "/users/42/posts/7/comments")
}
func BenchmarkDeepParamChi(b *testing.B) {
	run(b, buildChi(), "GET", "/users/42/posts/7/comments")
}
func BenchmarkDeepParamRouter(b *testing.B) {
	run(b, buildRouter(), "GET", "/users/42/posts/7/comments")
}
