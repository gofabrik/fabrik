// Package authsession is the one auth package that knows the core
// session library exists. It is both the sink login backends write
// identity through and the chain's single session-reading
// authenticator, so reading identity out of a session happens in
// exactly one place.
//
// Identity storage follows the session library's one-source rule:
// the session's UserID is the canonical auth key (provider + ":" +
// subject), and the non-id claims live in one private cell this
// package owns. A backend never learns the app's session type - the
// bridge takes the sealed [session.Registry]/[session.Lifecycle]
// view of the manager the app holds.
//
// The import path is auth/session but the package is named
// authsession, so consumers use it next to the core session package
// without aliasing either.
package authsession

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/session"
)

// reservedProvider is the provenance name a cell-less promoted
// identity reads back as; no real backend may claim it.
const reservedProvider = "session"

// cellData is the private identity cell: everything except the
// Subject, which is the session's UserID. Provider and Subject are
// stored only as a consistency check against that UserID, never as a
// second source of truth.
type cellData struct {
	Provider string
	Subject  string
	Email    string
	Name     string
	Claims   map[string]any
}

var key = session.NewKey[cellData]("github.com/gofabrik/fabrik/auth/session")

// Sessions is the manager view the bridge needs: cell registration
// plus the identity lifecycle. Every *session.Manager[T] satisfies
// it; a nil or typed-nil value is rejected at [New].
type Sessions interface {
	session.Registry
	session.Lifecycle
}

// Authenticator is the bridge. Construct it with [New]; hand its
// Login/Logout to a login backend as the sink, and put the
// Authenticator itself in an [auth.Chain] as the session leg.
type Authenticator struct {
	sessions Sessions
	cell     *session.Handle[cellData]
}

// New registers the identity cell on the manager and returns the
// bridge. It rejects a nil or typed-nil Sessions, and surfaces the
// cell registration error otherwise - those two are the whole error
// return. Calling New twice on the same manager is valid: the cell
// registration is idempotent for the same key, so the result is
// independent bridges over one cell.
func New(m Sessions) (*Authenticator, error) {
	if m == nil || isNilValue(m) {
		return nil, errors.New("session.New: nil Sessions")
	}
	cell, err := session.Use(m, key)
	if err != nil {
		return nil, err
	}
	return &Authenticator{sessions: m, cell: cell}, nil
}

// Login stages the visitor's identity into the session: the cell
// write and the Promote both stage and land in the request's single
// commit. The order (encode, then cell Save, then Promote) makes
// every partial state fail toward unauthenticated, and the cell's
// {Provider, Subject} consistency check catches an established
// session whose UserID and cell could otherwise diverge under a
// concurrent commit.
func (a *Authenticator) Login(ctx context.Context, id auth.Identity) error {
	uid, err := UserKey(id.Provider, id.Subject)
	if err != nil {
		return err
	}
	// Encode-before-stage: an unmarshalable claim aborts here, with
	// no session mutation at all.
	data := cellData{
		Provider: id.Provider,
		Subject:  id.Subject,
		Email:    id.Email,
		Name:     id.Name,
		Claims:   normalizeClaims(id.Claims),
	}
	if err := a.cell.Save(ctx, data); err != nil {
		return err
	}
	return a.sessions.Promote(ctx, uid)
}

// Logout ends the session. On a visitor with no live session it is
// idempotent success, riding the session library's own Destroy
// semantics.
func (a *Authenticator) Logout(ctx context.Context) error {
	return a.sessions.Destroy(ctx)
}

// Authenticate reads identity out of the session - the chain's
// session leg. The rules are precedence-ordered by the manager fact:
//
//   - UserID empty: ErrUnauthenticated, regardless of the cell (a
//     cell without a Promote decorates nothing).
//   - UserID set, cell present: the full identity, after the
//     consistency check UserKey(cell.Provider, cell.Subject) ==
//     UserID; a mismatch is corrupt state and fails closed.
//   - UserID set, no cell (an app-level Promote): a Subject-only
//     identity with Provider "session".
//   - session.ErrNoSession (no middleware): propagates as an error
//     and becomes the chain's 500 - a missing session middleware is
//     a wiring bug, loud on the first request.
func (a *Authenticator) Authenticate(r *http.Request) (auth.Identity, error) {
	ctx := r.Context()
	uid, err := a.sessions.UserID(ctx)
	if err != nil {
		if errors.Is(err, session.ErrNoSession) {
			return auth.Identity{}, err // fail closed: wiring bug
		}
		return auth.Identity{}, err
	}
	if uid == "" {
		return auth.Identity{}, auth.ErrUnauthenticated
	}

	// Ownership is decided by the cell's presence, never by parsing
	// the UserID: a bare Promote's UserID may itself contain ":"
	// (a tenant-scoped id), and the auth key is only ever composed.
	has, err := a.cell.Has(ctx)
	if err != nil {
		return auth.Identity{}, err // corrupt envelope: fail closed
	}
	if !has {
		// No auth cell: an app-level Promote. Subject-only,
		// provenance "session", whatever the UserID's shape.
		return auth.Identity{Subject: uid, Provider: reservedProvider}, nil
	}

	data, err := a.cell.Get(ctx)
	if err != nil {
		return auth.Identity{}, err // corrupt cell: fail closed
	}
	// Consistency check: the cell's composed key must equal the
	// session's UserID. A mismatch (a rare re-login staging race, or
	// store corruption) is treated as unauthenticated rather than a
	// hard error - it still refuses to serve the divergent identity,
	// but a 401/anonymous result lets a re-login or logout recover,
	// where a 500 would lock the session out until the cookie
	// expires.
	want, kerr := UserKey(data.Provider, data.Subject)
	if kerr != nil || want != uid {
		return auth.Identity{}, auth.ErrUnauthenticated
	}
	return auth.Identity{
		Subject:  data.Subject,
		Email:    data.Email,
		Name:     data.Name,
		Provider: data.Provider,
		Claims:   data.Claims,
	}, nil
}

// UserKey is the canonical auth-key definition: provider + ":" +
// subject. It rejects an empty Subject, an empty Provider, a
// Provider containing ":", and the reserved Provider "session". The
// key is only ever composed, never parsed against arbitrary input -
// subjects may contain ":" freely.
func UserKey(provider, subject string) (string, error) {
	switch {
	case subject == "":
		return "", errors.New("session: empty Subject")
	case provider == "":
		return "", errors.New("session: empty Provider")
	case strings.Contains(provider, ":"):
		return "", fmt.Errorf("session: Provider %q contains \":\"", provider)
	case provider == reservedProvider:
		return "", fmt.Errorf("session: Provider %q is reserved", reservedProvider)
	}
	return provider + ":" + subject, nil
}

// UserKey is the convenience over the package function for admin
// flows that already hold an [auth.Identity].
func (a *Authenticator) UserKey(id auth.Identity) (string, error) {
	return UserKey(id.Provider, id.Subject)
}

// normalizeClaims canonicalizes empty and nil to nil, so stored
// cells have one stable form.
func normalizeClaims(c map[string]any) map[string]any {
	if len(c) == 0 {
		return nil
	}
	return c
}

// isNilValue detects a typed-nil interface (var m *Manager[T] = nil
// passed as Sessions), which is non-nil as an interface but panics on
// use. Matches the session library's own typed-nil guard.
func isNilValue(m Sessions) bool {
	rv := reflect.ValueOf(m)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}
