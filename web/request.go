// Package web adapts typed response handlers to net/http.
//
// The request side is a light wrapper; full request typing belongs to form binding.
package web

import (
	"context"
	"net/http"
	"net/textproto"
	"time"
)

// Request wraps *http.Request with accessors and success-only response state.
type Request struct {
	r       *http.Request
	headers map[string]string
	cookies []*http.Cookie
}

func newRequest(r *http.Request) *Request {
	return &Request{r: r}
}

// HTTP returns the underlying request.
func (r *Request) HTTP() *http.Request { return r.r }

// Context returns the request context.
func (r *Request) Context() context.Context { return r.r.Context() }

// FormValue returns the named form or query value.
func (r *Request) FormValue(name string) string { return r.r.FormValue(name) }

// PathValue returns the named route wildcard value.
func (r *Request) PathValue(name string) string { return r.r.PathValue(name) }

// Query returns the named URL query value.
func (r *Request) Query(name string) string { return r.r.URL.Query().Get(name) }

// Cookie returns the named cookie's value and whether it was sent.
func (r *Request) Cookie(name string) (string, bool) {
	c, err := r.r.Cookie(name)
	if err != nil {
		return "", false
	}
	return c.Value, true
}

// SetHeader records a response header with Set semantics.
func (r *Request) SetHeader(key, value string) {
	if r.headers == nil {
		r.headers = map[string]string{}
	}
	r.headers[textproto.CanonicalMIMEHeaderKey(key)] = value
}

// CookieOption adjusts a cookie recorded by SetCookie.
type CookieOption func(*http.Cookie)

// CookieSecure marks the cookie Secure.
func CookieSecure() CookieOption { return func(c *http.Cookie) { c.Secure = true } }

// CookieHTTPOnly marks the cookie HttpOnly.
func CookieHTTPOnly() CookieOption { return func(c *http.Cookie) { c.HttpOnly = true } }

// CookieMaxAge sets the cookie lifetime.
func CookieMaxAge(d time.Duration) CookieOption {
	return func(c *http.Cookie) { c.MaxAge = int(d.Seconds()) }
}

// CookiePath overrides the default "/" path.
func CookiePath(path string) CookieOption {
	return func(c *http.Cookie) { c.Path = path }
}

// CookieSameSite sets the SameSite mode.
func CookieSameSite(mode http.SameSite) CookieOption {
	return func(c *http.Cookie) { c.SameSite = mode }
}

// SetCookie records a response cookie.
func (r *Request) SetCookie(name, value string, opts ...CookieOption) {
	c := &http.Cookie{Name: name, Value: value, Path: "/"}
	for _, opt := range opts {
		opt(c)
	}
	r.cookies = append(r.cookies, c)
}

// ClearCookie records the named cookie's deletion.
func (r *Request) ClearCookie(name string) {
	r.cookies = append(r.cookies, &http.Cookie{Name: name, Path: "/", MaxAge: -1})
}
