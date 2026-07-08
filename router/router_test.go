package router_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gofabrik/fabrik/router"
)

func do(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(method, target, nil))
	return rr
}

func body(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	b, _ := io.ReadAll(rr.Result().Body)
	return string(b)
}

func text(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, s) }
}

func mustPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatalf("%s: expected panic", name)
		}
	}()
	fn()
}

func TestStaticAndParam(t *testing.T) {
	r := router.New()
	r.Get("/", text("root"))
	r.Get("/users/{id}", func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, "user "+req.PathValue("id"))
	})

	if rr := do(t, r, "GET", "/"); rr.Code != 200 || body(t, rr) != "root" {
		t.Fatalf("GET / = %d %q", rr.Code, body(t, rr))
	}
	if rr := do(t, r, "GET", "/users/42"); rr.Code != 200 || body(t, rr) != "user 42" {
		t.Fatalf("GET /users/42 = %d %q", rr.Code, body(t, rr))
	}
}

func TestStaticBeatsParam(t *testing.T) {
	r := router.New()
	r.Get("/users/{id}", func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, "param:"+req.PathValue("id"))
	})
	r.Get("/users/new", text("static"))

	if rr := do(t, r, "GET", "/users/new"); body(t, rr) != "static" {
		t.Fatalf("static should win, got %q", body(t, rr))
	}
	if rr := do(t, r, "GET", "/users/7"); body(t, rr) != "param:7" {
		t.Fatalf("param route, got %q", body(t, rr))
	}
}

func TestWildcard(t *testing.T) {
	r := router.New()
	r.Get("/assets/{path...}", func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, req.PathValue("path"))
	})
	if rr := do(t, r, "GET", "/assets/css/site/app.css"); body(t, rr) != "css/site/app.css" {
		t.Fatalf("wildcard = %q", body(t, rr))
	}
}

func TestExactRootMatch(t *testing.T) {
	r := router.New()
	r.Get("/{$}", text("exact root"))
	r.Get("/other", text("other"))

	if rr := do(t, r, "GET", "/"); rr.Code != 200 || body(t, rr) != "exact root" {
		t.Fatalf("GET / = %d %q", rr.Code, body(t, rr))
	}
	if rr := do(t, r, "GET", "/nope"); rr.Code != http.StatusNotFound {
		t.Fatalf("GET /nope should 404, got %d", rr.Code)
	}
}

func TestTrailingSlashSubtreeAndRedirect(t *testing.T) {
	r := router.New()
	r.HandleFunc("/files/", func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, "file:"+req.URL.Path)
	})

	if rr := do(t, r, "GET", "/files/a/b.txt"); rr.Code != 200 || body(t, rr) != "file:/files/a/b.txt" {
		t.Fatalf("subtree = %d %q", rr.Code, body(t, rr))
	}
	// ServeMux redirects subtree roots to the trailing slash.
	rr := do(t, r, "GET", "/files")
	if rr.Code != http.StatusTemporaryRedirect || rr.Header().Get("Location") != "/files/" {
		t.Fatalf("want 307 -> /files/, got %d loc=%q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestNotFound(t *testing.T) {
	r := router.New()
	r.Get("/a", text("a"))
	if rr := do(t, r, "GET", "/nope"); rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rr.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	r := router.New()
	r.Get("/a", text("a"))
	r.Post("/a", text("a"))

	rr := do(t, r, "DELETE", "/a")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rr.Code)
	}
	if got := rr.Header().Get("Allow"); got != "GET, HEAD, POST" {
		t.Fatalf("Allow = %q, want \"GET, HEAD, POST\"", got)
	}
}

func TestAllowUnionsOverlappingPatterns(t *testing.T) {
	r := router.New()
	r.Get("/users/{id}", text("show"))
	r.Post("/users/new", text("create"))
	rr := do(t, r, "DELETE", "/users/new")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rr.Code)
	}
	if got := rr.Header().Get("Allow"); !strings.Contains(got, "GET") || !strings.Contains(got, "POST") {
		t.Fatalf("Allow = %q, want the union of GET (/users/{id}) and POST (/users/new)", got)
	}
}

func TestTrailingSlashMethodNotAllowed(t *testing.T) {
	r := router.New()
	r.Get("/files/", text("files"))
	if rr := do(t, r, "POST", "/files"); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /files want 405 (path+\"/\" match), got %d", rr.Code)
	}
}

func TestOptionsAndHead(t *testing.T) {
	r := router.New()
	r.Get("/a", text("a"))

	// ServeMux serves HEAD from the GET route.
	if rr := do(t, r, "HEAD", "/a"); rr.Code != 200 {
		t.Fatalf("HEAD want 200 (served by GET), got %d", rr.Code)
	}
	// OPTIONS without an explicit route is a 405 with Allow.
	rr := do(t, r, "OPTIONS", "/a")
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") == "" {
		t.Fatalf("OPTIONS = %d Allow=%q", rr.Code, rr.Header().Get("Allow"))
	}
}

func TestExplicitOptionsRoute(t *testing.T) {
	r := router.New()
	r.Get("/a", text("a"))
	r.Options("/a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Custom-Options", "1")
		io.WriteString(w, "opts")
	})
	rr := do(t, r, "OPTIONS", "/a")
	if rr.Code != 200 || body(t, rr) != "opts" || rr.Header().Get("X-Custom-Options") != "1" {
		t.Fatalf("explicit OPTIONS not served: %d %q", rr.Code, body(t, rr))
	}
}

func TestMethodAgnosticHandle(t *testing.T) {
	r := router.New()
	r.Handle("/any", text("any"))
	for _, m := range []string{"GET", "POST", "DELETE"} {
		if rr := do(t, r, m, "/any"); rr.Code != 200 || body(t, rr) != "any" {
			t.Fatalf("%s /any = %d %q", m, rr.Code, body(t, rr))
		}
	}
}

func TestExtensionMethods(t *testing.T) {
	// M-SEARCH pins valid method tokens with punctuation.
	methods := []string{"CONNECT", "PURGE", "M-SEARCH"}
	r := router.New()
	for _, m := range methods {
		r.Method(m, "/"+m, text(m))
	}
	for _, m := range methods {
		if rr := do(t, r, m, "/"+m); rr.Code != 200 || body(t, rr) != m {
			t.Fatalf("%s = %d %q", m, rr.Code, body(t, rr))
		}
	}
}

func TestCustomNotFound(t *testing.T) {
	r := router.New()
	r.Get("/a", text("a"))
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "nothing at "+req.URL.Path)
	})
	rr := do(t, r, "GET", "/nope")
	if rr.Code != http.StatusNotFound || body(t, rr) != "nothing at /nope" {
		t.Fatalf("custom 404 = %d %q", rr.Code, body(t, rr))
	}
}

func TestCustomMethodNotAllowedGetsAllow(t *testing.T) {
	r := router.New()
	r.Get("/a", text("a"))
	r.Post("/a", text("a"))
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		io.WriteString(w, "try: "+w.Header().Get("Allow"))
	})
	rr := do(t, r, "DELETE", "/a")
	if rr.Code != http.StatusMethodNotAllowed || body(t, rr) != "try: GET, HEAD, POST" {
		t.Fatalf("custom 405 = %d %q (Allow=%q)", rr.Code, body(t, rr), rr.Header().Get("Allow"))
	}
}

func TestCustomNotFoundIgnoresAppWritten404(t *testing.T) {
	// Matched handlers own their own status codes.
	r := router.New()
	r.Get("/item/{id}", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "item gone", http.StatusNotFound)
	})
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "router 404")
	})

	if rr := do(t, r, "GET", "/item/5"); body(t, rr) != "item gone\n" {
		t.Fatalf("app 404 was intercepted: %q", body(t, rr))
	}
	if rr := do(t, r, "GET", "/missing"); body(t, rr) != "router 404" {
		t.Fatalf("router 404 not used for real miss: %q", body(t, rr))
	}
}

func TestGlobalMiddlewareWrapsCustomErrors(t *testing.T) {
	r := router.New()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Global", "1")
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/a", text("a"))
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })

	if got := do(t, r, "GET", "/nope").Header().Get("X-Global"); got != "1" {
		t.Fatalf("global middleware did not wrap custom 404")
	}
}

func TestCustomErrorHandlersDefaultStatus(t *testing.T) {
	// Body-only handlers still send the routing status.
	r := router.New()
	r.Get("/a", text("a"))
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "gone") })
	r.MethodNotAllowed(func(http.ResponseWriter, *http.Request) {})

	if rr := do(t, r, "GET", "/nope"); rr.Code != http.StatusNotFound || body(t, rr) != "gone" {
		t.Fatalf("body-only NotFound = %d %q, want 404 gone", rr.Code, body(t, rr))
	}
	rr := do(t, r, "POST", "/a")
	if rr.Code != http.StatusMethodNotAllowed || body(t, rr) != "" {
		t.Fatalf("empty MethodNotAllowed = %d %q, want 405 and empty body", rr.Code, body(t, rr))
	}
	if rr.Header().Get("Allow") == "" {
		t.Fatalf("empty MethodNotAllowed dropped Allow header")
	}
}

func TestCustomErrorHandlerCanOverrideStatus(t *testing.T) {
	r := router.New()
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "boom")
	})
	if rr := do(t, r, "GET", "/nope"); rr.Code != http.StatusInternalServerError || body(t, rr) != "boom" {
		t.Fatalf("override = %d %q, want 500 boom", rr.Code, body(t, rr))
	}
}

func TestErrorHooksFallBackToStdlib(t *testing.T) {
	// Unset error hooks fall back to ServeMux.
	r := router.New()
	r.Get("/a", text("a"))
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })

	rr := do(t, r, "POST", "/a")
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") == "" {
		t.Fatalf("405 fallback = %d Allow=%q", rr.Code, rr.Header().Get("Allow"))
	}
}

func TestGlobalMiddlewareWrapsMisses(t *testing.T) {
	r := router.New()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Global", "1")
			next.ServeHTTP(w, req)
		})
	})
	r.Get("/a", text("a"))

	for _, tc := range []struct{ method, path string }{
		{"GET", "/a"},
		{"GET", "/nope"},
		{"POST", "/a"},
	} {
		rr := do(t, r, tc.method, tc.path)
		if rr.Header().Get("X-Global") != "1" {
			t.Fatalf("global middleware did not run for %s %s (code %d)", tc.method, tc.path, rr.Code)
		}
	}
}

func TestScopedMiddleware(t *testing.T) {
	mark := func(v string) router.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Add("X-Mark", v)
				next.ServeHTTP(w, req)
			})
		}
	}
	r := router.New()
	r.With(mark("a")).Get("/a", text("a"))
	r.Get("/b", text("b"))

	if got := do(t, r, "GET", "/a").Header().Values("X-Mark"); len(got) != 1 || got[0] != "a" {
		t.Fatalf("/a marks = %v", got)
	}
	if got := do(t, r, "GET", "/b").Header().Values("X-Mark"); len(got) != 0 {
		t.Fatalf("/b should have no marks, got %v", got)
	}
}

func TestMiddlewareOrder(t *testing.T) {
	var order []string
	mk := func(name string) router.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, req)
			})
		}
	}
	r := router.New()
	r.Use(mk("g1"), mk("g2"))
	r.With(mk("w1")).Get("/a", func(w http.ResponseWriter, req *http.Request) {
		order = append(order, "handler")
	})

	do(t, r, "GET", "/a")
	if got := strings.Join(order, ","); got != "g1,g2,w1,handler" {
		t.Fatalf("middleware order = %q, want g1,g2,w1,handler", got)
	}
}

func TestRouteGroupPrefixAndMiddleware(t *testing.T) {
	mark := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Grp", "1")
			next.ServeHTTP(w, req)
		})
	}
	r := router.New()
	r.Route("/api", func(r *router.Scope) {
		r.Use(mark)
		r.Get("/ping", text("pong"))
	})
	r.Get("/ping", text("root pong"))

	rr := do(t, r, "GET", "/api/ping")
	if rr.Code != 200 || body(t, rr) != "pong" || rr.Header().Get("X-Grp") != "1" {
		t.Fatalf("/api/ping = %d %q grp=%q", rr.Code, body(t, rr), rr.Header().Get("X-Grp"))
	}
	rr = do(t, r, "GET", "/ping")
	if rr.Code != 200 || body(t, rr) != "root pong" || rr.Header().Get("X-Grp") != "" {
		t.Fatalf("/ping leaked group scope: grp=%q", rr.Header().Get("X-Grp"))
	}
}

func TestNestedGroupScoping(t *testing.T) {
	mark := func(v string) router.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Add("X-M", v)
				next.ServeHTTP(w, req)
			})
		}
	}
	r := router.New()
	r.Route("/api", func(r *router.Scope) {
		r.Use(mark("api"))
		r.Route("/v1", func(r *router.Scope) {
			r.Use(mark("v1"))
			r.Get("/x", text("x"))
		})
		r.Get("/ping", text("ping"))
	})

	rr := do(t, r, "GET", "/api/v1/x")
	if got := strings.Join(rr.Header().Values("X-M"), ","); got != "api,v1" {
		t.Fatalf("/api/v1/x marks = %q, want api,v1", got)
	}
	rr = do(t, r, "GET", "/api/ping")
	if got := strings.Join(rr.Header().Values("X-M"), ","); got != "api" {
		t.Fatalf("/api/ping marks = %q, want api (v1 leaked)", got)
	}
}

func TestExactRootUnderGroupAndMount(t *testing.T) {
	// Grouped exact-root routes land on the prefix itself.
	r := router.New()
	r.Route("/api", func(r *router.Scope) {
		r.Get("/{$}", text("api root"))
	})
	if rr := do(t, r, "GET", "/api"); rr.Code != 200 || body(t, rr) != "api root" {
		t.Fatalf("GET /api = %d %q, want exact-root match", rr.Code, body(t, rr))
	}
	if rr := do(t, r, "GET", "/api/x"); rr.Code != http.StatusNotFound {
		t.Fatalf("GET /api/x should 404, got %d", rr.Code)
	}

	// Mounted exact-root routes follow the same rule.
	sub := router.New()
	sub.Get("/{$}", text("svc root"))
	m := router.New()
	m.Mount("/svc", sub)
	if rr := do(t, m, "GET", "/svc"); rr.Code != 200 || body(t, rr) != "svc root" {
		t.Fatalf("GET /svc = %d %q, want exact-root match", rr.Code, body(t, rr))
	}
}

func TestNestedMount(t *testing.T) {
	inner := router.New()
	inner.Get("/{iid}", func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, req.PathValue("tid")+"/"+req.PathValue("pid")+"/"+req.PathValue("iid"))
	})
	mid := router.New()
	mid.Mount("/posts/{pid}/items", inner)

	root := router.New()
	root.Mount("/tenants/{tid}", mid)

	rr := do(t, root, "GET", "/tenants/1/posts/2/items/3")
	if rr.Code != 200 || body(t, rr) != "1/2/3" {
		t.Fatalf("nested mount = %d %q, want 1/2/3 (params preserved across mounts)", rr.Code, body(t, rr))
	}
}

func TestMountAtRoot(t *testing.T) {
	sub := router.New()
	sub.Get("/ping", text("pong"))

	r := router.New()
	r.Mount("/", sub)

	rr := do(t, r, "GET", "/ping")
	if rr.Code != 200 || body(t, rr) != "pong" {
		t.Fatalf("mount at root = %d %q", rr.Code, body(t, rr))
	}
	for _, rt := range r.Routes() {
		if strings.Contains(rt.Pattern, "//") {
			t.Fatalf("double slash in pattern: %q", rt.Pattern)
		}
	}
}

func TestHandleWithStripPrefix(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, "path="+req.URL.Path)
	})
	r := router.New()
	r.Handle("/static/", http.StripPrefix("/static", inner))

	rr := do(t, r, "GET", "/static/css/app.css")
	if rr.Code != 200 || body(t, rr) != "path=/css/app.css" {
		t.Fatalf("Handle+StripPrefix = %d %q, want stripped path", rr.Code, body(t, rr))
	}
}

func TestRoutesIntrospection(t *testing.T) {
	r := router.New()
	r.Get("/a", text("a"))
	r.Post("/b", text("b"))
	got := r.Routes()
	if len(got) != 2 || got[0] != (router.Route{Method: "GET", Pattern: "/a"}) || got[1] != (router.Route{Method: "POST", Pattern: "/b"}) {
		t.Fatalf("Routes() = %+v", got)
	}
}

func TestZeroValueRouterPanicsClearly(t *testing.T) {
	// A zero-value Router reports constructor misuse.
	for name, fn := range map[string]func(){
		"Get":    func() { var r router.Router; r.Get("/a", text("a")) },
		"Use":    func() { var r router.Router; r.Use(func(h http.Handler) http.Handler { return h }) },
		"Route":  func() { var r router.Router; r.Route("/a", func(*router.Scope) {}) },
		"Serve":  func() { var r router.Router; do(t, &r, "GET", "/") },
		"NotFnd": func() { var r router.Router; r.NotFound(func(http.ResponseWriter, *http.Request) {}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				msg, _ := recover().(string)
				if !strings.Contains(msg, "router.New") {
					t.Fatalf("panic = %q, want a router.New message", msg)
				}
			}()
			fn()
		})
	}
}

func TestOptionsAsteriskUnderHooks(t *testing.T) {
	// The hook path must match ServeMux: OPTIONS * is a 400, not a redirect.
	r := router.New()
	r.Get("/a", text("a"))
	r.NotFound(func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest("OPTIONS", "/", nil)
	req.RequestURI = "*"
	req.URL.Path = "*"
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("OPTIONS * = %d, want 400 (got Location=%q)", rr.Code, rr.Header().Get("Location"))
	}
}

func TestMethodHandle(t *testing.T) {
	r := router.New()
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "purged")
	})
	r.MethodHandle("PURGE", "/cache", h)

	if rr := do(t, r, "PURGE", "/cache"); rr.Code != 200 || body(t, rr) != "purged" {
		t.Fatalf("PURGE /cache = %d %q", rr.Code, body(t, rr))
	}
	if rr := do(t, r, "GET", "/cache"); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /cache should 405, got %d", rr.Code)
	}
}

func TestWalk(t *testing.T) {
	mark := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("X-Scoped", "1")
			next.ServeHTTP(w, req)
		})
	}
	raw := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	r := router.New()
	r.Get("/a", raw)
	r.With(mark).Post("/b", raw)

	type row struct{ method, pattern string }
	var order []row
	r.Walk(func(method, pattern string, handler, wrapped http.Handler) {
		order = append(order, row{method, pattern})
		// The raw handler is what was registered; the wrapped handler runs the
		// scoped middleware.
		if pattern == "/b" {
			rr := httptest.NewRecorder()
			wrapped.ServeHTTP(rr, httptest.NewRequest("POST", "/b", nil))
			if rr.Header().Get("X-Scoped") != "1" {
				t.Fatalf("/b wrapped handler missing scoped middleware")
			}
		}
	})

	want := []row{{"GET", "/a"}, {"POST", "/b"}}
	if len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("Walk order = %+v, want %+v", order, want)
	}
}

func TestPanics(t *testing.T) {
	cases := map[string]func(){
		"empty method":          func() { router.New().Method("", "/a", text("a")) },
		"method with space":     func() { router.New().Method("GET POST", "/a", text("a")) },
		"pattern no slash":      func() { router.New().Get("nope", text("a")) },
		"route prefix no slash": func() { router.New().Route("api", func(*router.Scope) {}) },
		"mount prefix no slash": func() { router.New().Mount("api", router.New()) },
		"nil subrouter mount":   func() { router.New().Mount("/x", nil) },
		"zero-value subrouter":  func() { router.New().Mount("/x", &router.Router{}) },
		"invalid method handle": func() { router.New().MethodHandle("GET POST", "/x", text("a")) },
		"nil group callback":    func() { router.New().Group(nil) },
		"nil route callback":    func() { router.New().Route("/x", nil) },
		"duplicate route":       func() { r := router.New(); r.Get("/a", text("a")); r.Get("/a", text("a")) },
		"self mount":            func() { r := router.New(); r.Mount("/x", r) },
		"conflicting patterns": func() {
			// ServeMux rejects conflicting patterns at registration.
			r := router.New()
			r.Get("/a/{x}", text("x"))
			r.Get("/a/{y}", text("y"))
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) { mustPanic(t, name, fn) })
	}
}

func TestNilHandlerAndMiddlewarePanic(t *testing.T) {
	mustPanic(t, "nil Get handler", func() { router.New().Get("/x", nil) })
	mustPanic(t, "nil Method handler", func() { router.New().Method("GET", "/x", nil) })
	mustPanic(t, "nil HandleFunc", func() { router.New().HandleFunc("/x", nil) })
	mustPanic(t, "nil Handle", func() { router.New().Handle("/x", nil) })
	mustPanic(t, "nil MethodHandle", func() { router.New().MethodHandle("GET", "/x", nil) })
	mustPanic(t, "nil Use middleware", func() { router.New().Use(nil) })
	mustPanic(t, "nil With middleware", func() { router.New().With(nil) })
	mustPanic(t, "nil route middleware", func() { router.New().Method("GET", "/x", text("x"), nil) })
	mustPanic(t, "nil NotFound", func() { router.New().NotFound(nil) })
	mustPanic(t, "nil MethodNotAllowed", func() { router.New().MethodNotAllowed(nil) })
	// Typed nil handlers can hide inside a non-nil interface.
	mustPanic(t, "typed-nil Handle", func() { var h http.HandlerFunc; router.New().Handle("/x", h) })
}

func TestMiddlewareReturningNilPanics(t *testing.T) {
	returnNil := func(http.Handler) http.Handler { return nil }
	returnTypedNil := func(http.Handler) http.Handler { var h http.HandlerFunc; return h }

	// Scoped middleware is composed at registration.
	mustPanic(t, "route returns nil", func() { router.New().Method("GET", "/x", text("x"), returnNil) })
	mustPanic(t, "route returns typed nil", func() { router.New().Method("GET", "/x", text("x"), returnTypedNil) })
}

func TestRegisterAfterServePanics(t *testing.T) {
	r := router.New()
	r.Get("/a", text("a"))
	_ = do(t, r, "GET", "/a")

	passthrough := func(h http.Handler) http.Handler { return h }
	mustPanic(t, "late route", func() { r.Get("/b", text("b")) })
	mustPanic(t, "late Use", func() { r.Use(passthrough) })
	mustPanic(t, "late With", func() { r.With(passthrough) })
	mustPanic(t, "late NotFound", func() { r.NotFound(func(http.ResponseWriter, *http.Request) {}) })
	mustPanic(t, "late MethodNotAllowed", func() { r.MethodNotAllowed(func(http.ResponseWriter, *http.Request) {}) })
	// Late scopes panic even when they register no routes.
	mustPanic(t, "late Group", func() { r.Group(func(*router.Scope) {}) })
	mustPanic(t, "late Route", func() { r.Route("/x", func(*router.Scope) {}) })
	mustPanic(t, "late Mount", func() { r.Mount("/x", router.New()) })
}

func TestGlobalMiddlewareBuiltOnce(t *testing.T) {
	// Error hooks do not rebuild global middleware.
	hooks := map[string]func(*router.Router){
		"NotFound":         func(r *router.Router) { r.NotFound(func(http.ResponseWriter, *http.Request) {}) },
		"MethodNotAllowed": func(r *router.Router) { r.MethodNotAllowed(func(http.ResponseWriter, *http.Request) {}) },
	}
	for name, hook := range hooks {
		t.Run(name, func(t *testing.T) {
			built := 0
			mw := func(next http.Handler) http.Handler {
				built++
				return next
			}
			r := router.New()
			r.Use(mw)
			hook(r)
			r.Get("/a", text("a"))
			do(t, r, "GET", "/a")
			do(t, r, "GET", "/a")
			if built != 1 {
				t.Fatalf("global middleware constructed %d times, want 1", built)
			}
		})
	}
}

func TestFreezeRetriesAfterPanic(t *testing.T) {
	// Failed freeze attempts can be retried without reopening registration.
	fail := true
	mw := func(next http.Handler) http.Handler {
		if fail {
			return nil
		}
		return next
	}
	r := router.New()
	r.Use(mw)
	r.Get("/a", text("a"))

	mustPanic(t, "first serve", func() { do(t, r, "GET", "/a") })
	// Failed freeze attempts still seal registration.
	mustPanic(t, "register after failed freeze", func() { r.Get("/b", text("b")) })

	fail = false
	rr := do(t, r, "GET", "/a")
	if rr.Code != 200 || body(t, rr) != "a" {
		t.Fatalf("retry after freeze panic = %d %q, want 200 a", rr.Code, body(t, rr))
	}
}

func TestConcurrentFirstServe(t *testing.T) {
	// Concurrent first requests compose global middleware once.
	var built atomic.Int64
	r := router.New()
	r.Use(func(next http.Handler) http.Handler {
		built.Add(1)
		return next
	})
	r.Get("/a", text("ok"))

	const n = 64
	var wg sync.WaitGroup
	start := make(chan struct{})
	fails := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rr := do(t, r, "GET", "/a")
			if b := body(t, rr); rr.Code != 200 || b != "ok" {
				fails <- fmt.Sprintf("%d %q", rr.Code, b)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(fails)

	for f := range fails {
		t.Errorf("concurrent serve failed: %s", f)
	}
	if got := built.Load(); got != 1 {
		t.Fatalf("global middleware constructed %d times, want 1", got)
	}
}

func TestGlobalMiddlewareReturningNilPanicsOnServe(t *testing.T) {
	// Global middleware is composed lazily on first serve.
	mws := map[string]router.Middleware{
		"nil":       func(http.Handler) http.Handler { return nil },
		"typed nil": func(http.Handler) http.Handler { var h http.HandlerFunc; return h },
	}
	for name, mw := range mws {
		t.Run(name, func(t *testing.T) {
			r := router.New()
			r.Use(mw)
			r.Get("/a", text("a"))
			mustPanic(t, "serve", func() { do(t, r, "GET", "/a") })
		})
	}
}
