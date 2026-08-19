package auth

import (
	"net/http"

	"github.com/gofabrik/fabrik/authn"
	"github.com/gofabrik/fabrik/authn/session"
	"github.com/gofabrik/fabrik/forms"
	"github.com/gofabrik/fabrik/validation"
	"github.com/gofabrik/fabrik/web"
)

// ErrorPage is the view model for 401 and 403 error pages.
type ErrorPage struct {
	Status int
}

// LoginInput is the login form.
type LoginInput struct {
	Username string
	Password string
}

func (in LoginInput) Validate() validation.Errors {
	return validation.Check(
		validation.Field("username", in.Username, validation.Required()),
		validation.Field("password", in.Password, validation.Required()),
	)
}

// LoginForm is the login page's view model.
type LoginForm struct {
	Form  *forms.Form[LoginInput]
	Error string
}

type Handlers struct {
	Auth *session.Auth
}

//fabrik:web GET /login middleware=nocache
func (h *Handlers) ShowLogin(req *web.Request) (web.Response, error) {
	return web.Template("auth/login", LoginForm{Form: forms.Empty[LoginInput]()}), nil
}

//fabrik:web POST /login middleware=nocache
func (h *Handlers) Login(req *web.Request) (web.Response, error) {
	form, err := forms.Bind[LoginInput](req.HTTP())
	if err != nil {
		return nil, err
	}
	if !form.Valid() {
		return web.Template("auth/login", LoginForm{Form: form}).Status(http.StatusUnprocessableEntity), nil
	}

	var claims *authn.ClaimSet
	switch form.Data.Username {
	case "admin":
		if form.Data.Password == "admin" {
			claims = &authn.ClaimSet{Subject: "admin", Roles: []string{"admin"}}
		}
	case "viewer":
		if form.Data.Password == "viewer" {
			claims = &authn.ClaimSet{Subject: "viewer", Roles: []string{"viewer"}}
		}
	}

	if claims == nil {
		return web.Template("auth/login", LoginForm{Form: form, Error: "invalid credentials"}).Status(http.StatusUnprocessableEntity), nil
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
