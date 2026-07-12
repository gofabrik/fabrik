package session

import (
	"context"
	"net/http"
	"reflect"
)

// Manager persists the app's session struct per visitor.
//
//	type Session struct {
//		Name string
//	}
//
//	sessions, err := session.New[Session](session.Config{...})
//	handler := sessions.Middleware(mux)
//
//	s, err := sessions.Get(ctx)
//	s.Name = "alice"
//	err = sessions.Save(ctx, s)
//
// T lives in the reserved app cell, so type renames do not orphan
// live sessions.
//
// T persists through encoding/json. Construction checks the type
// shape; marshal failures surface at Save, Get, or Update.
//
// Manager also satisfies [Registry] for session-consuming libraries.
type Manager[T any] struct {
	c   *core
	app *Handle[T]
}

// New validates cfg and returns the session manager. T must be a
// struct; every configuration problem is reported in one pass.
func New[T any](cfg Config) (*Manager[T], error) {
	if err := checkCellType(reflect.TypeFor[T](), "New"); err != nil {
		return nil, err
	}
	c, err := newCore(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.register(appKey, reflect.TypeFor[T]()); err != nil {
		return nil, err
	}
	return &Manager[T]{
		c:   c,
		app: &Handle[T]{m: c, key: appKey},
	}, nil
}

// registry seals [Registry] to Manager values.
func (m *Manager[T]) registry() *core {
	if m == nil {
		return nil
	}
	return m.c
}

// lifecycle seals [Lifecycle] the same way.
func (m *Manager[T]) lifecycle() *core {
	if m == nil {
		return nil
	}
	return m.c
}

// Middleware attaches request session state and commits it at
// response start.
func (m *Manager[T]) Middleware(next http.Handler) http.Handler { return m.c.Middleware(next) }

// Get returns the request-current session data. Absence reads as T's
// zero value.
func (m *Manager[T]) Get(ctx context.Context) (T, error) { return m.app.Get(ctx) }

// Has reports whether session data exists, including staged writes.
func (m *Manager[T]) Has(ctx context.Context) (bool, error) { return m.app.Has(ctx) }

// Save stages session data for the response-start commit.
func (m *Manager[T]) Save(ctx context.Context, v T) error { return m.app.Save(ctx, v) }

// Update applies fn under optimistic CAS and writes immediately.
func (m *Manager[T]) Update(ctx context.Context, fn func(*T) error) error {
	return m.app.Update(ctx, fn)
}

// Clear stages removal of the app data without ending the session.
func (m *Manager[T]) Clear(ctx context.Context) error { return m.app.Clear(ctx) }

// SID returns the session ID the request arrived with.
func (m *Manager[T]) SID(ctx context.Context) (string, error) { return m.c.SID(ctx) }

// UserID returns the request-current session user ID.
func (m *Manager[T]) UserID(ctx context.Context) (string, error) { return m.c.UserID(ctx) }

// Promote stages login and rotates the SID.
func (m *Manager[T]) Promote(ctx context.Context, userID string) error {
	return m.c.Promote(ctx, userID)
}

// Renew stages an SID rotation without extending absolute expiry.
func (m *Manager[T]) Renew(ctx context.Context) error { return m.c.Renew(ctx) }

// Destroy stages logout and leaves the request sessionless.
func (m *Manager[T]) Destroy(ctx context.Context) error { return m.c.Destroy(ctx) }

// Load reads app data by SID without request middleware.
func (m *Manager[T]) Load(ctx context.Context, sid string) (T, error) {
	return m.app.Load(ctx, sid)
}

// UpdateSID updates app data by SID without creating a session.
func (m *Manager[T]) UpdateSID(ctx context.Context, sid string, fn func(*T) error) error {
	return m.app.UpdateSID(ctx, sid, fn)
}

// ClearSID removes app data by SID without ending the session.
func (m *Manager[T]) ClearSID(ctx context.Context, sid string) error {
	return m.app.ClearSID(ctx, sid)
}

// DestroySID revokes one session. Revocation is idempotent.
func (m *Manager[T]) DestroySID(ctx context.Context, sid string) error {
	return m.c.DestroySID(ctx, sid)
}

// ListForUser returns the SIDs of every live session belonging to
// userID. Requires a store with the [UserIndexer] capability.
func (m *Manager[T]) ListForUser(ctx context.Context, userID string) ([]string, error) {
	return m.c.ListForUser(ctx, userID)
}

// RevokeAllForUser deletes every live session for userID except the
// optional SIDs.
func (m *Manager[T]) RevokeAllForUser(ctx context.Context, userID string, except ...string) (int, error) {
	return m.c.RevokeAllForUser(ctx, userID, except...)
}
