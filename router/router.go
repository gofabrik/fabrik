// Package router adds middleware, groups, mounting, and error hooks on top of
// http.ServeMux.
package router

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Middleware wraps an http.Handler with additional behavior.
type Middleware func(http.Handler) http.Handler

// Route is a registered method and pattern. An empty Method means the route
// matches any method.
type Route struct {
	Method  string
	Pattern string
}

// entry keeps raw route data so Mount can rebase subrouter routes.
type entry struct {
	method  string
	pattern string
	handler http.Handler // as registered
	wrapped http.Handler // handler with route middleware applied
	mws     []Middleware
}

type state struct {
	mux              *http.ServeMux
	entries          []entry
	global           []Middleware
	notFound         http.HandlerFunc
	methodNotAllowed http.HandlerFunc
	handler          http.Handler
	buildMu          sync.Mutex
	sealed           atomic.Bool // registration is closed
	built            atomic.Bool // handler is composed
}

// Scope is a registration handle. It is not an http.Handler.
type Scope struct {
	state  *state
	prefix string
	inline []Middleware
}

// Router is the servable entry point. Construct it with New.
type Router struct {
	scope Scope
}

// New returns an empty Router.
func New() *Router {
	return &Router{scope: Scope{state: &state{mux: http.NewServeMux()}}}
}

// st returns the shared state, reporting a zero-value Router built without New.
func (s *Scope) st() *state {
	if s.state == nil {
		panic("router: use router.New to construct a Router")
	}
	return s.state
}

func (s *Scope) Get(pattern string, h http.HandlerFunc)     { s.Method(http.MethodGet, pattern, h) }
func (s *Scope) Post(pattern string, h http.HandlerFunc)    { s.Method(http.MethodPost, pattern, h) }
func (s *Scope) Put(pattern string, h http.HandlerFunc)     { s.Method(http.MethodPut, pattern, h) }
func (s *Scope) Patch(pattern string, h http.HandlerFunc)   { s.Method(http.MethodPatch, pattern, h) }
func (s *Scope) Delete(pattern string, h http.HandlerFunc)  { s.Method(http.MethodDelete, pattern, h) }
func (s *Scope) Head(pattern string, h http.HandlerFunc)    { s.Method(http.MethodHead, pattern, h) }
func (s *Scope) Options(pattern string, h http.HandlerFunc) { s.Method(http.MethodOptions, pattern, h) }

// Method registers h for the given HTTP method and pattern, wrapped in mw
// (outermost first) on top of any middleware in scope. The method may be any
// valid HTTP method token, including CONNECT, TRACE, and extension methods.
func (s *Scope) Method(verb, pattern string, h http.HandlerFunc, mw ...Middleware) {
	if !validMethod(verb) {
		panic("router: invalid method \"" + verb + "\"")
	}
	if h == nil {
		panic("router: nil handler for " + verb + " " + pattern)
	}
	s.register(verb, pattern, h, mw)
}

// Handle registers a method-agnostic handler for pattern.
func (s *Scope) Handle(pattern string, h http.Handler) {
	if isNilHandler(h) {
		panic("router: nil handler for " + pattern)
	}
	s.register("", pattern, h, nil)
}

// HandleFunc registers a method-agnostic handler func for pattern.
func (s *Scope) HandleFunc(pattern string, h http.HandlerFunc) {
	if h == nil {
		panic("router: nil handler for " + pattern)
	}
	s.register("", pattern, h, nil)
}

// MethodHandle registers an http.Handler for the given method and pattern. It is
// Method for callers that already hold an http.Handler.
func (s *Scope) MethodHandle(method, pattern string, h http.Handler) {
	if !validMethod(method) {
		panic("router: invalid method \"" + method + "\"")
	}
	if isNilHandler(h) {
		panic("router: nil handler for " + method + " " + pattern)
	}
	s.register(method, pattern, h, nil)
}

func (s *Scope) register(method, pattern string, h http.Handler, mw []Middleware) {
	if !strings.HasPrefix(pattern, "/") {
		panic("router: pattern must start with \"/\": " + pattern)
	}
	checkMiddleware(mw)
	s.add(entry{
		method:  method,
		pattern: joinPattern(s.prefix, pattern),
		handler: h,
		mws:     append(slices.Clone(s.inline), mw...),
	})
}

// add registers immediately so ServeMux validates patterns at the call site.
func (s *Scope) add(e entry) {
	if s.st().sealed.Load() {
		panic("router: cannot register routes after serving has started")
	}
	key := e.pattern
	if e.method != "" {
		key = e.method + " " + e.pattern
	}
	e.wrapped = chain(e.mws, e.handler)
	s.state.mux.Handle(key, e.wrapped)
	s.state.entries = append(s.state.entries, e)
}

// Use adds scoped middleware to routes registered afterward.
func (s *Scope) Use(mw ...Middleware) {
	if s.st().sealed.Load() {
		panic("router: cannot add middleware after serving has started")
	}
	checkMiddleware(mw)
	s.inline = append(s.inline, mw...)
}

// With returns a scope that applies mw to routes registered on it.
func (s *Scope) With(mw ...Middleware) *Scope {
	if s.st().sealed.Load() {
		panic("router: cannot add middleware after serving has started")
	}
	checkMiddleware(mw)
	c := s.child(s.prefix)
	c.inline = append(c.inline, mw...)
	return c
}

// Group registers routes that share the current scope's middleware.
func (s *Scope) Group(fn func(s *Scope)) {
	if s.st().sealed.Load() {
		panic("router: cannot register routes after serving has started")
	}
	if fn == nil {
		panic("router: nil group callback")
	}
	fn(s.child(s.prefix))
}

// Route registers routes under a path prefix with the current scope's middleware.
func (s *Scope) Route(prefix string, fn func(s *Scope)) {
	if s.st().sealed.Load() {
		panic("router: cannot register routes after serving has started")
	}
	if fn == nil {
		panic("router: nil route callback")
	}
	if !strings.HasPrefix(prefix, "/") {
		panic("router: route prefix must start with \"/\": " + prefix)
	}
	fn(s.child(s.prefix + prefix))
}

func (s *Scope) child(prefix string) *Scope {
	return &Scope{state: s.state, prefix: strings.TrimSuffix(prefix, "/"), inline: slices.Clone(s.inline)}
}

// Mount flattens sub's routes under prefix.
//
// The subrouter is copied as it is at the time of the call. Its miss handlers are
// not copied, and its global middleware is folded into each mounted route.
//
// Attach arbitrary handlers with Handle:
//
//	s.Handle("/static/", http.StripPrefix("/static", fileServer))
func (s *Scope) Mount(prefix string, sub *Router) {
	if s.st().sealed.Load() {
		panic("router: cannot register routes after serving has started")
	}
	if !strings.HasPrefix(prefix, "/") {
		panic("router: mount prefix must start with \"/\": " + prefix)
	}
	if sub == nil {
		panic("router: nil subrouter for mount " + prefix)
	}
	if sub.scope.state == nil {
		panic("router: invalid subrouter for mount " + prefix)
	}
	if sub.scope.state == s.state {
		panic("router: cannot mount a router into itself")
	}
	base := strings.TrimSuffix(s.prefix+prefix, "/")
	for _, e := range sub.scope.state.entries {
		s.add(entry{
			method:  e.method,
			pattern: joinPattern(base, e.pattern),
			handler: e.handler,
			mws:     slices.Concat(s.inline, sub.scope.state.global, e.mws),
		})
	}
}

func (r *Router) Get(pattern string, h http.HandlerFunc)     { r.scope.Get(pattern, h) }
func (r *Router) Post(pattern string, h http.HandlerFunc)    { r.scope.Post(pattern, h) }
func (r *Router) Put(pattern string, h http.HandlerFunc)     { r.scope.Put(pattern, h) }
func (r *Router) Patch(pattern string, h http.HandlerFunc)   { r.scope.Patch(pattern, h) }
func (r *Router) Delete(pattern string, h http.HandlerFunc)  { r.scope.Delete(pattern, h) }
func (r *Router) Head(pattern string, h http.HandlerFunc)    { r.scope.Head(pattern, h) }
func (r *Router) Options(pattern string, h http.HandlerFunc) { r.scope.Options(pattern, h) }

func (r *Router) Method(verb, pattern string, h http.HandlerFunc, mw ...Middleware) {
	r.scope.Method(verb, pattern, h, mw...)
}
func (r *Router) Handle(pattern string, h http.Handler)         { r.scope.Handle(pattern, h) }
func (r *Router) HandleFunc(pattern string, h http.HandlerFunc) { r.scope.HandleFunc(pattern, h) }
func (r *Router) MethodHandle(method, pattern string, h http.Handler) {
	r.scope.MethodHandle(method, pattern, h)
}
func (r *Router) With(mw ...Middleware) *Scope           { return r.scope.With(mw...) }
func (r *Router) Group(fn func(s *Scope))                { r.scope.Group(fn) }
func (r *Router) Route(prefix string, fn func(s *Scope)) { r.scope.Route(prefix, fn) }
func (r *Router) Mount(prefix string, sub *Router)       { r.scope.Mount(prefix, sub) }

// Use adds global middleware. It wraps every request, including 404/405.
func (r *Router) Use(mw ...Middleware) {
	st := r.scope.st()
	if st.sealed.Load() {
		panic("router: cannot add middleware after serving has started")
	}
	checkMiddleware(mw)
	st.global = append(st.global, mw...)
}

// NotFound sets the handler for requests that match no route. Its response
// defaults to 404 (a body-only handler need not call WriteHeader). Global
// middleware still wraps it. Matched handlers that write 404 are unaffected.
func (r *Router) NotFound(h http.HandlerFunc) { r.setErrorHandler(&r.scope.st().notFound, h) }

// MethodNotAllowed sets the handler for requests whose path matches a route under
// a different method; the Allow header is set before it runs and the response
// defaults to 405. Global middleware still wraps it.
func (r *Router) MethodNotAllowed(h http.HandlerFunc) {
	r.setErrorHandler(&r.scope.st().methodNotAllowed, h)
}

func (r *Router) setErrorHandler(dst *http.HandlerFunc, h http.HandlerFunc) {
	st := r.scope.state
	if st.sealed.Load() {
		panic("router: cannot set an error handler after serving has started")
	}
	if h == nil {
		panic("router: nil error handler")
	}
	*dst = h
}

// core uses the bare mux when no error hooks are set.
func (r *Router) core() http.Handler {
	st := r.scope.state
	if st.notFound == nil && st.methodNotAllowed == nil {
		return st.mux
	}
	return http.HandlerFunc(r.serveWithErrorHooks)
}

func (r *Router) serveWithErrorHooks(w http.ResponseWriter, req *http.Request) {
	st := r.scope.state
	// OPTIONS * must go through ServeHTTP to preserve its 400.
	if req.RequestURI == "*" {
		st.mux.ServeHTTP(w, req)
		return
	}
	h, pattern := st.mux.Handler(req)
	if pattern != "" {
		st.mux.ServeHTTP(w, req)
		return
	}
	// Probe the miss response to preserve its status classification and Allow header.
	probe := errorProbe{header: http.Header{}}
	h.ServeHTTP(&probe, req)
	switch probe.status {
	case http.StatusMethodNotAllowed:
		if st.methodNotAllowed != nil {
			if a := probe.header.Get("Allow"); a != "" {
				w.Header().Set("Allow", a)
			}
			runErrorHandler(w, req, http.StatusMethodNotAllowed, st.methodNotAllowed)
			return
		}
	case http.StatusNotFound:
		if st.notFound != nil {
			runErrorHandler(w, req, http.StatusNotFound, st.notFound)
			return
		}
	}
	h.ServeHTTP(w, req)
}

// Routes returns the registered routes in registration order.
func (r *Router) Routes() []Route {
	st := r.scope.st()
	out := make([]Route, 0, len(st.entries))
	for _, e := range st.entries {
		out = append(out, Route{Method: e.method, Pattern: e.pattern})
	}
	return out
}

// Walk calls fn for each registered route in registration order. handler is the
// handler as registered; wrapped has the route's scoped middleware applied, but
// not the global middleware, which wraps every request. An empty method means the
// route matches any method.
func (r *Router) Walk(fn func(method, pattern string, handler, wrapped http.Handler)) {
	for _, e := range r.scope.st().entries {
		fn(e.method, e.pattern, e.handler, e.wrapped)
	}
}

// ServeHTTP freezes registration on first use, then dispatches requests.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	st := r.scope.st()
	if !st.built.Load() {
		r.freeze()
	}
	st.handler.ServeHTTP(w, req)
}

// freeze seals registration before composing. A failed compose can be retried.
func (r *Router) freeze() {
	st := r.scope.state
	st.buildMu.Lock()
	defer st.buildMu.Unlock()
	st.sealed.Store(true)
	if st.built.Load() {
		return
	}
	handler := chain(st.global, r.core())
	st.handler = handler
	st.built.Store(true)
}

// runErrorHandler defaults body-only error handlers to the routing status.
func runErrorHandler(w http.ResponseWriter, req *http.Request, code int, h http.HandlerFunc) {
	dw := &defaultStatusWriter{ResponseWriter: w, code: code}
	h(dw, req)
	if !dw.wrote {
		dw.WriteHeader(code)
	}
}

type defaultStatusWriter struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (w *defaultStatusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *defaultStatusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(w.code)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *defaultStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// errorProbe captures miss status and headers without writing a body.
type errorProbe struct {
	header http.Header
	status int
}

func (p *errorProbe) Header() http.Header { return p.header }
func (p *errorProbe) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}

func (p *errorProbe) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(b), nil
}

// joinPattern maps "/{$}" under a prefix to the prefix itself.
func joinPattern(base, pattern string) string {
	if base != "" && pattern == "/{$}" {
		return base
	}
	return base + pattern
}

// chain wraps h in mw, outermost first.
func chain(mw []Middleware, h http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
		if isNilHandler(h) {
			panic("router: middleware returned a nil handler")
		}
	}
	return h
}

func checkMiddleware(mw []Middleware) {
	for _, m := range mw {
		if m == nil {
			panic("router: nil middleware")
		}
	}
}

// isNilHandler catches typed nil handlers stored in an interface.
func isNilHandler(h http.Handler) bool {
	if h == nil {
		return true
	}
	switch v := reflect.ValueOf(h); v.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Chan, reflect.Slice, reflect.Interface:
		return v.IsNil()
	}
	return false
}

// validMethod reports whether m is a non-empty HTTP method token per RFC 7230.
func validMethod(m string) bool {
	if m == "" {
		return false
	}
	for i := 0; i < len(m); i++ {
		if !isTokenByte(m[i]) {
			return false
		}
	}
	return true
}

func isTokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' ||
		c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' ||
		strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}
