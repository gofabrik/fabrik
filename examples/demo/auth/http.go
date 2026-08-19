package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/gofabrik/fabrik/authn"
	"github.com/gofabrik/fabrik/authn/password"
	"github.com/gofabrik/fabrik/authn/session"
	"github.com/gofabrik/fabrik/forms"
	"github.com/gofabrik/fabrik/ratelimit"
	"github.com/gofabrik/fabrik/validation"
	"github.com/gofabrik/fabrik/web"
)

// ErrorPage is the view model for 401 and 403 error pages.
type ErrorPage struct {
	Status int
}

// LoginInput is the login form.
type LoginInput struct {
	Email    string
	Password string
}

func (in LoginInput) Validate() validation.Errors {
	return validation.Check(
		validation.Field("email", in.Email, validation.Required(), validation.Email(), validation.MaxLen(254)),
		validation.Field("password", in.Password, validation.Required()),
	)
}

// LoginForm is the login page's view model.
type LoginForm struct {
	Form  *forms.Form[LoginInput]
	Error string
}

type Handlers struct {
	Auth     *session.Auth
	Verifier *password.Verifier
	Limiter  *ratelimit.Limiter
}

//fabrik:web GET /login middleware=nocache
func (h *Handlers) ShowLogin(req *web.Request) (web.Response, error) {
	return web.Template("auth/login", LoginForm{Form: forms.Empty[LoginInput]()}), nil
}

//fabrik:web POST /login middleware=nocache,loginlimit
func (h *Handlers) Login(req *web.Request) (web.Response, error) {
	form, err := forms.Bind[LoginInput](req.HTTP())
	if err != nil {
		return nil, err
	}
	if !form.Valid() {
		return web.Template("auth/login", LoginForm{Form: form}).Status(http.StatusUnprocessableEntity), nil
	}

	email := password.NormalizeEmail(form.Data.Email)
	sum := sha256.Sum256([]byte(email))
	key := hex.EncodeToString(sum[:])

	result, err := h.Limiter.Allow(req.Context(), key)
	if err != nil {
		return nil, err
	}
	if !result.Allowed {
		return web.Template("auth/login", LoginForm{Form: form, Error: "too many attempts"}).Status(http.StatusTooManyRequests), nil
	}

	claims, err := h.Verifier.Authenticate(req.Context(), email, form.Data.Password)
	if err != nil {
		if errors.Is(err, password.ErrInvalidCredentials) {
			return web.Template("auth/login", LoginForm{Form: form, Error: "invalid credentials"}).Status(http.StatusUnprocessableEntity), nil
		}
		return nil, err
	}

	if err := h.Auth.Login(req.Context(), claims); err != nil {
		return nil, err
	}
	return web.Redirect("/"), nil
}

//fabrik:web POST /logout
func (h *Handlers) Logout(req *web.Request) (web.Response, error) {
	if err := h.Auth.Logout(req.Context()); err != nil {
		return nil, err
	}
	return web.Redirect("/"), nil
}

//fabrik:web GET /private middleware=nocache,authenticated
func (h *Handlers) Private(req *web.Request) (web.Response, error) {
	c, _ := authn.Claims(req.Context())
	return web.Template("auth/private", c), nil
}

//fabrik:web GET /admin middleware=nocache,authenticated,admin
func (h *Handlers) Admin(req *web.Request) (web.Response, error) {
	c, _ := authn.Claims(req.Context())
	return web.Template("auth/admin", c), nil
}
