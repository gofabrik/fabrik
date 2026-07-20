package session

import (
	"net/http"
	"time"
)

// Cookie is a [Token] that reads and writes the session ID as an HTTP
// cookie. Empty Name defaults to "sid"; empty Path defaults to "/".
//
// The zero value is not production-safe: Secure, HttpOnly, and
// SameSite are all off unless set. A production deployment sets at
// least HttpOnly and Secure, and usually SameSite:
//
//	session.Cookie{Name: "session", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
//
// Cookie does not sign or encrypt the session ID. The ID is an
// opaque lookup key.
type Cookie struct {
	Name     string
	Path     string
	Domain   string
	Secure   bool
	HttpOnly bool //nolint:revive // mirrors net/http.Cookie.HttpOnly (public API)
	SameSite http.SameSite
}

// defaultCookieName is used when [Cookie.Name] is empty.
const defaultCookieName = "sid"

func (c Cookie) name() string {
	if c.Name == "" {
		return defaultCookieName
	}
	return c.Name
}

// Read returns the configured cookie's session ID. Multiple matching
// cookies follow net/http's first-match behavior.
func (c Cookie) Read(r *http.Request) (string, bool) {
	ck, err := r.Cookie(c.name())
	if err != nil || ck.Value == "" {
		return "", false
	}
	return ck.Value, true
}

// Write emits a Set-Cookie header carrying sid. If opts.Expiry is
// non-zero, both the Expires and MaxAge attributes are set from it.
func (c Cookie) Write(w http.ResponseWriter, sid string, opts TokenWriteOptions) {
	// #nosec G124 -- cookie attributes are caller-configurable
	ck := &http.Cookie{
		Name:     c.name(),
		Value:    sid,
		Path:     c.cookiePath(),
		Domain:   c.Domain,
		Secure:   c.Secure,
		HttpOnly: c.HttpOnly,
		SameSite: c.SameSite,
	}
	if !opts.Expiry.IsZero() {
		ck.Expires = opts.Expiry
		// MaxAge wins over Expires in modern browsers.
		now := opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		if d := opts.Expiry.Sub(now); d > 0 {
			ck.MaxAge = int(d.Seconds())
		}
	}
	http.SetCookie(w, ck)
}

// Clear emits a Set-Cookie header with an empty value and an
// already-elapsed expiry, instructing the client to discard the
// cookie immediately.
func (c Cookie) Clear(w http.ResponseWriter) {
	// #nosec G124 -- cookie attributes are caller-configurable
	http.SetCookie(w, &http.Cookie{
		Name:     c.name(),
		Value:    "",
		Path:     c.cookiePath(),
		Domain:   c.Domain,
		Secure:   c.Secure,
		HttpOnly: c.HttpOnly,
		SameSite: c.SameSite,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	})
}

func (c Cookie) cookiePath() string {
	if c.Path == "" {
		return "/"
	}
	return c.Path
}
