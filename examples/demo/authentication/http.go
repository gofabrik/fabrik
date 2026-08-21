package authentication

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/password"
	"github.com/gofabrik/fabrik/auth/session"
	"github.com/gofabrik/fabrik/auth/store"
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

//fabrik:inject IPLimiter name=loginip
//fabrik:inject RegisterLimiter name=registerip
type Handlers struct {
	Auth            *session.Auth
	Verifier        *password.Verifier
	Accounts        *Accounts
	Limiter         *ratelimit.Limiter
	IPLimiter       *ratelimit.Limiter
	RegisterLimiter *ratelimit.Limiter
}

//fabrik:web GET /login middleware=nocache
func (h *Handlers) ShowLogin(req *web.Request) (web.Response, error) {
	return web.Template("auth/login", LoginForm{Form: forms.Empty[LoginInput]()}), nil
}

//fabrik:web POST /login middleware=nocache
func (h *Handlers) Login(req *web.Request) (web.Response, error) {
	// Missing IP keys and limiter failures fail closed with 503.
	ip := ratelimit.KeyByIP(req.HTTP())
	if ip == "" {
		return web.Template("auth/login", LoginForm{Form: forms.Empty[LoginInput](), Error: "temporarily unavailable"}).Status(http.StatusServiceUnavailable), nil
	}
	ipResult, err := h.IPLimiter.Allow(req.Context(), ip)
	if err != nil {
		return web.Template("auth/login", LoginForm{Form: forms.Empty[LoginInput](), Error: "temporarily unavailable"}).Status(http.StatusServiceUnavailable), nil
	}
	// Handler responses, including redirects, carry quota headers.
	// Adapter-rendered errors do not preserve request headers.
	req.SetHeader("RateLimit-Limit", strconv.Itoa(ipResult.Limit))
	req.SetHeader("RateLimit-Remaining", strconv.Itoa(ipResult.Remaining))
	req.SetHeader("RateLimit-Reset", strconv.Itoa(int(math.Ceil(ipResult.ResetAfter.Seconds()))))
	if !ipResult.Allowed {
		req.SetHeader("Retry-After", strconv.Itoa(int(math.Ceil(ipResult.RetryAfter.Seconds()))))
		return web.Template("auth/login", LoginForm{Form: forms.Empty[LoginInput](), Error: "too many requests"}).Status(http.StatusTooManyRequests), nil
	}

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

// RegisterInput is the registration form, with maximum lengths measured in
// bytes and the password minimum measured in characters.
type RegisterInput struct {
	Email    string
	Password string
	Confirm  string
}

func (in RegisterInput) Validate() validation.Errors {
	return validation.Check(
		validation.Field("email", in.Email, validation.Required(), validation.Email(),
			validation.By(func(v string) error {
				if len(v) > password.MaxEmailLen {
					return fmt.Errorf("must be at most %d bytes", password.MaxEmailLen)
				}
				return nil
			})),
		validation.Field("password", in.Password, validation.Required(), validation.MinLen(15),
			validation.By(func(v string) error {
				if len(v) > password.MaxPasswordLen {
					return fmt.Errorf("must be at most %d bytes", password.MaxPasswordLen)
				}
				return nil
			})),
		validation.Field("confirm", in.Confirm, validation.By(func(v string) error {
			if v != in.Password {
				return errors.New("passwords do not match")
			}
			return nil
		})),
	)
}

// RegisterForm is the registration page's view model.
type RegisterForm struct {
	Form  *forms.Form[RegisterInput]
	Error string
}

//fabrik:web GET /register middleware=nocache
func (h *Handlers) ShowRegister(req *web.Request) (web.Response, error) {
	return web.Template("auth/register", RegisterForm{Form: forms.Empty[RegisterInput]()}), nil
}

//fabrik:web POST /register middleware=nocache
func (h *Handlers) Register(req *web.Request) (web.Response, error) {
	// Missing IP keys and limiter failures fail closed with 503.
	ip := ratelimit.KeyByIP(req.HTTP())
	if ip == "" {
		return web.Template("auth/register", RegisterForm{Form: forms.Empty[RegisterInput](), Error: "temporarily unavailable"}).Status(http.StatusServiceUnavailable), nil
	}
	ipResult, err := h.RegisterLimiter.Allow(req.Context(), ip)
	if err != nil {
		return web.Template("auth/register", RegisterForm{Form: forms.Empty[RegisterInput](), Error: "temporarily unavailable"}).Status(http.StatusServiceUnavailable), nil
	}
	req.SetHeader("RateLimit-Limit", strconv.Itoa(ipResult.Limit))
	req.SetHeader("RateLimit-Remaining", strconv.Itoa(ipResult.Remaining))
	req.SetHeader("RateLimit-Reset", strconv.Itoa(int(math.Ceil(ipResult.ResetAfter.Seconds()))))
	if !ipResult.Allowed {
		req.SetHeader("Retry-After", strconv.Itoa(int(math.Ceil(ipResult.RetryAfter.Seconds()))))
		return web.Template("auth/register", RegisterForm{Form: forms.Empty[RegisterInput](), Error: "too many requests"}).Status(http.StatusTooManyRequests), nil
	}

	form, err := forms.Bind[RegisterInput](req.HTTP())
	if err != nil {
		return nil, err
	}
	if !form.Valid() {
		return web.Template("auth/register", RegisterForm{Form: form}).Status(http.StatusUnprocessableEntity), nil
	}

	hash, err := h.Verifier.Hash(req.Context(), form.Data.Password)
	if err != nil {
		return nil, err
	}
	email := password.NormalizeEmail(form.Data.Email)
	claims, err := h.Accounts.Register(req.Context(), email, hash)
	if err != nil {
		if errors.Is(err, store.ErrEmailTaken) {
			return web.Template("auth/register", RegisterForm{Form: form, Error: "that email is already registered"}).Status(http.StatusUnprocessableEntity), nil
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
	c, _ := auth.Claims(req.Context())
	return web.Template("auth/private", c), nil
}

//fabrik:web GET /admin middleware=nocache,authenticated,admin
func (h *Handlers) Admin(req *web.Request) (web.Response, error) {
	c, _ := auth.Claims(req.Context())
	return web.Template("auth/admin", c), nil
}
