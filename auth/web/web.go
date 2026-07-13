// Package web is the batteries-included, server-rendered UI for
// password auth: login, register, logout, and account pages with a
// working POST-redirect-GET flow, over any [password.Store] and
// [password.Sink]. Every seam is overridable through [Options], but
// set none of them and it just works.
//
//	ui, err := web.New(store, sink, web.Options{
//		Auth: auth.Required(chain), // protects the account page
//	})
//	mux.Handle("/auth/", ui.Handler()) // login/register/logout/account
//
// The UI owns the [password.Provider] and wires its success/failure
// to redirects and inline form errors. An API-only app skips this
// package and uses [password.New] directly.
//
// Self-service registration is inherently email-enumerable: a new
// email is created and logged in (a redirect), a taken one cannot be
// (the form re-renders), and no message wording hides that
// difference. This UI is the auto-login batteries flow; an app that
// must resist enumeration - or wants email verification, or any other
// no-auto-login registration - skips this package and drives
// [password.Provider] directly, where an OnRegistered hook can
// withhold the login and render a uniform "check your email"
// response.
//
// CSRF is not handled here. The shipped forms carry no CSRF token;
// a SameSite=Lax (or Strict) session cookie is the baseline that
// blocks cross-site logout and post-login CSRF, and token-based CSRF
// is the app's to add (or the csrf library's once it lands). Set the
// cookie's SameSite when you configure the session.
package web

import (
	"cmp"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"strings"

	fauth "github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/password"
	"github.com/gofabrik/fabrik/templates"
)

// all: keeps the _layout.html and any underscore partials that a plain
// embed would drop.
//
//go:embed all:templates
var templatesFS embed.FS

// Source returns the embedded auth templates as a layer source: an "auth"
// section with a layout and login/register/account pages. A bundle stacks
// the app's trees on top of this base so [templates.LoadLayers] can
// override an individual page or the whole layout.
func Source() templates.Source {
	return templates.Source{FS: templatesFS, Dir: "templates"}
}

// Options configures a [UI]. The zero value is valid: every field
// has a working default.
type Options struct {
	// Prefix is the mount path for the UI's routes and the form
	// action URLs. Default "/auth" - mount Handler() at the same
	// prefix.
	Prefix string

	// Auth protects the account page (typically auth.Required(chain)).
	// Nil disables the built-in /account route.
	Auth func(http.Handler) http.Handler

	// RateLimit wraps the credential POSTs (login, register). Nil
	// leaves them unthrottled - bounded only by bcrypt latency.
	RateLimit func(http.Handler) http.Handler

	// AfterLogin / AfterLogout are the redirect targets on success.
	// Defaults: Prefix+"/account" and "/".
	AfterLogin  string
	AfterLogout string

	// DisableRegister omits the register routes.
	DisableRegister bool

	// MinPasswordLength is the register-time minimum (default 8).
	MinPasswordLength int

	// Hasher overrides the password hasher (default bcrypt cost 12).
	// Set it to tune the cost or supply a custom algorithm without
	// leaving this UI; nil keeps the default.
	Hasher password.Hasher

	// Message maps a backend failure to user-facing form text. Nil
	// uses [DefaultMessage].
	Message func(error) string

	// Templates renders the auth pages. It must provide the
	// "auth/login", "auth/register", and "auth/account" page keys -
	// typically an app-merged set carrying auth's own templates
	// ([Source]) as the base layer. Nil builds a standalone set from the
	// embedded defaults.
	Templates *templates.Set
}

// UI is the server-rendered auth surface. Build it with [New] and
// mount [UI.Handler].
type UI struct {
	provider *password.Provider
	set      *templates.Set

	prefix      string
	afterLogin  string
	afterLogout string
	auth        func(http.Handler) http.Handler
	rateLimit   func(http.Handler) http.Handler
	disableReg  bool
	message     func(error) string
}

// New builds the UI over store and sink. It constructs the
// [password.Provider] internally, wiring its success and failure to
// redirects and inline form errors.
func New(store password.Store, sink password.Sink, opts Options) (*UI, error) {
	ui := &UI{
		prefix:      cmp.Or(opts.Prefix, "/auth"),
		afterLogout: cmp.Or(opts.AfterLogout, "/"),
		auth:        opts.Auth,
		rateLimit:   opts.RateLimit,
		disableReg:  opts.DisableRegister,
		message:     opts.Message,
	}
	ui.afterLogin = cmp.Or(opts.AfterLogin, ui.prefix+"/account")
	if ui.message == nil {
		ui.message = DefaultMessage
	}

	ui.set = opts.Templates
	if ui.set == nil {
		set, err := templates.Load(templatesFS, "templates")
		if err != nil {
			return nil, fmt.Errorf("authweb.New: load templates: %w", err)
		}
		ui.set = set
	}

	p, err := password.New(store, sink, password.Options{
		MinPasswordLength: opts.MinPasswordLength,
		Hasher:            opts.Hasher,
		OnSuccess:         ui.onSuccess,
		OnFailure:         ui.onFailure,
	})
	if err != nil {
		return nil, err
	}
	ui.provider = p
	return ui, nil
}

// Handler returns the UI's routes as one http.Handler, ready to
// mount at Prefix: GET/POST login, GET/POST register (unless
// disabled), POST logout, and GET account (if Auth is set).
func (ui *UI) Handler() http.Handler {
	mux := http.NewServeMux()
	p := ui.prefix

	mux.HandleFunc("GET "+p+"/login", ui.page("login"))
	mux.Handle("POST "+p+"/login", ui.throttle(http.HandlerFunc(ui.provider.Login)))
	if !ui.disableReg {
		mux.HandleFunc("GET "+p+"/register", ui.page("register"))
		mux.Handle("POST "+p+"/register", ui.throttle(http.HandlerFunc(ui.provider.Register)))
	}
	mux.HandleFunc("POST "+p+"/logout", ui.provider.Logout)
	if ui.auth != nil {
		mux.Handle("GET "+p+"/account", ui.auth(http.HandlerFunc(ui.account)))
	}
	return mux
}

func (ui *UI) throttle(h http.Handler) http.Handler {
	if ui.rateLimit == nil {
		return h
	}
	return ui.rateLimit(h)
}

// onSuccess redirects: to AfterLogin on login/register, AfterLogout
// on logout (the zero identity).
func (ui *UI) onSuccess(w http.ResponseWriter, r *http.Request, id fauth.Identity) error {
	dest := ui.afterLogin
	if id.Subject == "" {
		dest = ui.afterLogout
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
	return nil
}

// onFailure routes a backend failure. An operational failure (store
// outage, hashing, a failed session write or logout) is a 500 - not
// a form error dressed as "invalid credentials", which would lie to
// the user and hide the fault from monitoring. A credential or
// validation failure re-renders the submitted form with the message
// inline, preserving the email so the user does not retype it.
func (ui *UI) onFailure(w http.ResponseWriter, r *http.Request, err error) {
	if password.IsOperational(err) {
		http.Error(w, "Something went wrong. Please try again.", http.StatusInternalServerError)
		return
	}
	name := "login"
	if strings.HasSuffix(r.URL.Path, "/register") {
		name = "register"
	}
	ui.render(w, name, pageData{
		Prefix: ui.prefix,
		Error:  ui.message(err),
		Email:  r.FormValue("email"),
	})
}

func (ui *UI) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ui.render(w, name, pageData{Prefix: ui.prefix})
	}
}

func (ui *UI) account(w http.ResponseWriter, r *http.Request) {
	id, _ := fauth.FromContext(r.Context())
	ui.render(w, "account", pageData{Prefix: ui.prefix, Email: id.Email})
}

func (ui *UI) render(w http.ResponseWriter, page string, data pageData) {
	// The login page links to register only when registration is on,
	// so disabling it never renders a link to a 404.
	data.Register = !ui.disableReg
	// Set.Render buffers, so an error writes nothing and the 500 is clean.
	if err := ui.set.Render(w, "auth/"+page, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
}

type pageData struct {
	Prefix   string
	Error    string
	Email    string
	Register bool // registration enabled; gates the login page's register link
}

// DefaultMessage turns a backend failure into form text - specific
// where it helps the user, generic otherwise. Note the generic
// register message is defense-in-depth only: the HTTP outcome still
// distinguishes a taken email (see the package doc on enumeration).
func DefaultMessage(err error) string {
	var tooShort *password.PasswordTooShortError
	var tooLong *password.PasswordTooLongError
	switch {
	case errors.As(err, &tooShort):
		return fmt.Sprintf("Password must be at least %d characters.", tooShort.Min)
	case errors.As(err, &tooLong):
		return fmt.Sprintf("Password must be at most %d characters.", tooLong.Max)
	case errors.Is(err, password.ErrEmailTaken):
		return "Could not create that account."
	default:
		return "Invalid email or password."
	}
}
