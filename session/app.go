package session

import (
	"context"
	"reflect"
)

// pinApp validates and fixes the app-cell type before request or store access.
func pinApp[T any](m *Manager) error {
	t := reflect.TypeFor[T]()
	if p := m.appT.Load(); p != nil && *p == t {
		return nil
	}
	if err := checkCellType(t, "app-cell access"); err != nil {
		return err
	}
	if err := m.register(appKey, t); err != nil {
		return err
	}
	m.appT.Store(&t)
	return nil
}

func appHandle[T any](m *Manager) *Handle[T] {
	return &Handle[T]{m: m, key: appKey}
}

// Get returns request-scoped app data, or T's zero value when absent.
func (m *Manager) Get[T any](ctx context.Context) (T, error) {
	if err := pinApp[T](m); err != nil {
		var zero T
		return zero, err
	}
	return appHandle[T](m).Get(ctx)
}

// Save stages app data for the response-start commit.
func (m *Manager) Save[T any](ctx context.Context, v T) error {
	if err := pinApp[T](m); err != nil {
		return err
	}
	return appHandle[T](m).Save(ctx, v)
}

// Update applies fn to app data and persists it immediately with optimistic concurrency.
func (m *Manager) Update[T any](ctx context.Context, fn func(*T) error) error {
	if err := pinApp[T](m); err != nil {
		return err
	}
	return appHandle[T](m).Update(ctx, fn)
}

// Load returns app data for SID without request middleware.
func (m *Manager) Load[T any](ctx context.Context, sid string) (T, error) {
	if err := pinApp[T](m); err != nil {
		var zero T
		return zero, err
	}
	return appHandle[T](m).Load(ctx, sid)
}

// UpdateSID updates existing app data for SID without creating a session.
func (m *Manager) UpdateSID[T any](ctx context.Context, sid string, fn func(*T) error) error {
	if err := pinApp[T](m); err != nil {
		return err
	}
	return appHandle[T](m).UpdateSID(ctx, sid, fn)
}
