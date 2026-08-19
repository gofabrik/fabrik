// Package session authenticates requests from claims stored in session state.
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gofabrik/fabrik/authn"
	"github.com/gofabrik/fabrik/session"
)

// Manager provides the session operations required by Auth.
// External implementations must embed a session.Registry.
type Manager interface {
	session.Registry
	Promote(ctx context.Context, userID string) error
	Destroy(ctx context.Context) error
	UserID(ctx context.Context) (string, error)
}

// Options configures an Auth.
type Options struct {
	// Logger receives session read failures. A nil Logger uses the default logger.
	Logger *slog.Logger
}

// Auth authenticates requests from session state.
type Auth struct {
	m    Manager
	cell *session.Handle[stored]
	log  *slog.Logger
}

const cellName = "github.com/gofabrik/fabrik/authn/session"

type stored struct {
	Claims authn.ClaimSet `json:"claims"`
}

// New constructs an Auth and registers its private session storage on m.
func New(m Manager, opts Options) (*Auth, error) {
	h, err := session.Use[stored](m, cellName)
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Auth{m: m, cell: h, log: log}, nil
}

// Login stores a snapshot of c and promotes the session to c.Subject,
// rotating the session ID.
//
// The snapshot omits exp and nbf because the session lifetime controls
// validity. Other claim changes take effect at the next login.
//
// State commits when the response starts, so Login must run first. If
// saving after promotion fails, Login destroys the session on a best-effort
// basis to prevent stale claims from authenticating the new identity.
func (a *Auth) Login(ctx context.Context, c *authn.ClaimSet) error {
	if c == nil || c.Subject == "" {
		return errors.New("authn/session: Login requires claims with a subject")
	}
	if err := a.m.Promote(ctx, c.Subject); err != nil {
		return fmt.Errorf("authn/session: promote: %w", err)
	}
	snap := *c
	snap.Expiry = nil
	snap.NotBefore = nil
	if err := a.cell.Save(ctx, stored{Claims: snap}); err != nil {
		if derr := a.m.Destroy(ctx); derr != nil {
			a.log.WarnContext(ctx, "session auth: cleanup destroy failed", "error", derr)
		}
		return fmt.Errorf("authn/session: store claims: %w", err)
	}
	return nil
}

// Logout destroys the session. It is a no-op without session state.
func (a *Auth) Logout(ctx context.Context) error {
	if err := a.m.Destroy(ctx); err != nil && !errors.Is(err, session.ErrNoSession) {
		return err
	}
	return nil
}

// Authenticate returns stored claims or (nil, nil) when the request has no
// session claims. Store and decode failures are errors. A user ID that does
// not match the stored subject is also an error because the session identity
// is authoritative.
func (a *Auth) Authenticate(r *http.Request) (*authn.ClaimSet, error) {
	v, err := a.cell.Get(r.Context())
	if err != nil {
		if errors.Is(err, session.ErrNoSession) {
			return nil, nil
		}
		return nil, err
	}
	if v.Claims.Subject == "" {
		return nil, nil
	}
	uid, err := a.m.UserID(r.Context())
	if err != nil {
		return nil, err
	}
	if uid != v.Claims.Subject {
		return nil, fmt.Errorf("authn/session: session user %q does not match the stored subject %q", uid, v.Claims.Subject)
	}
	c := v.Claims
	return &c, nil
}

// Middleware attaches session claims unless the request already carries
// claims. Read failures are logged and treated as anonymous requests.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authn.Claims(r.Context()); !ok {
			c, err := a.Authenticate(r)
			switch {
			case err != nil:
				a.log.WarnContext(r.Context(), "session auth: read failed, continuing anonymous", "error", err)
			case c != nil:
				r = r.WithContext(authn.WithClaims(r.Context(), c))
			}
		}
		next.ServeHTTP(w, r)
	})
}
