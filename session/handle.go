package session

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
)

// Registry is the sealed library-facing view of a session [Manager].
//
//	func New(m session.Registry) (*CSRF, error) {
//		h, err := session.Use(m, key)
//		...
//	}
type Registry interface {
	registry() *core
}

// Lifecycle is the sealed capability for libraries that manage the
// session's identity - the auth bridge is its first consumer. Like
// [Registry] it is satisfied only by [Manager] (the unexported
// method is the seal), which also means it can grow without
// breaking anyone. Libraries compose the two by embedding:
//
//	func New(m interface { session.Registry; session.Lifecycle }) ...
type Lifecycle interface {
	Promote(ctx context.Context, userID string) error
	Destroy(ctx context.Context) error
	UserID(ctx context.Context) (string, error)
	lifecycle() *core
}

// Use registers a typed library cell and returns its handle.
//
// The same [Key] value is idempotent; the same name with a different
// type is an error. Cell values persist through encoding/json.
//
// Use is concurrency-safe and intended at wiring time.
func Use[T any](m Registry, key Key[T]) (*Handle[T], error) {
	if key.name == "" {
		return nil, fmt.Errorf("session.Use: zero Key (declare one with session.NewKey)")
	}
	t := reflect.TypeFor[T]()
	if err := checkCellType(t, "Use"); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("session.Use: nil Registry")
	}
	c := m.registry()
	if c == nil {
		return nil, fmt.Errorf("session.Use: Registry carries no session manager (typed nil?)")
	}
	if err := c.register(key.name, t); err != nil {
		return nil, err
	}
	return &Handle[T]{m: c, key: key.name}, nil
}

// Handle is typed access to one session cell.
type Handle[T any] struct {
	m   *core
	key string
}

// Key returns the handle's resolved cell key.
func (h *Handle[T]) Key() string { return h.key }

// Get returns the cell's request-current value. Absence reads as T's
// zero value.
func (h *Handle[T]) Get(ctx context.Context) (T, error) {
	var zero T
	raw, ok, err := h.m.cellGet(ctx, h.key)
	if err != nil || !ok {
		return zero, err
	}
	return h.decode(raw)
}

// Has reports request-current existence, including staged writes.
func (h *Handle[T]) Has(ctx context.Context) (bool, error) {
	return h.m.cellHas(ctx, h.key)
}

// Save stages a whole-cell overwrite.
func (h *Handle[T]) Save(ctx context.Context, v T) error {
	if err := h.m.checkStageable(ctx, "Save"); err != nil {
		return err
	}
	raw, err := h.encode(v)
	if err != nil {
		return err
	}
	return h.m.cellSave(ctx, h.key, raw)
}

// Update applies fn under optimistic CAS and writes immediately.
//
// fn may re-run after a conflict and must be self-contained.
func (h *Handle[T]) Update(ctx context.Context, fn func(*T) error) error {
	return h.m.cellUpdate(ctx, h.key, h.rawFn(fn))
}

// Clear stages removal of the cell without ending the session.
func (h *Handle[T]) Clear(ctx context.Context) error {
	return h.m.cellClear(ctx, h.key)
}

// Load reads the cell by SID without request middleware.
func (h *Handle[T]) Load(ctx context.Context, sid string) (T, error) {
	var zero T
	raw, ok, err := h.m.loadCell(ctx, sid, h.key)
	if err != nil || !ok {
		return zero, err
	}
	return h.decode(raw)
}

// UpdateSID updates the cell by SID without creating a session.
func (h *Handle[T]) UpdateSID(ctx context.Context, sid string, fn func(*T) error) error {
	return h.m.updateCellSID(ctx, sid, h.key, h.rawFn(fn))
}

// rawFn bridges a typed closure to raw cell bytes.
func (h *Handle[T]) rawFn(fn func(*T) error) func(prev cellRaw) (cellRaw, error) {
	return func(prev cellRaw) (cellRaw, error) {
		v, err := h.decodePrev(prev)
		if err != nil {
			return nil, err
		}
		if err := fn(&v); err != nil {
			return nil, err
		}
		return h.encode(v)
	}
}

// ClearSID removes the cell by SID. Setting a zero value is not
// clearing.
func (h *Handle[T]) ClearSID(ctx context.Context, sid string) error {
	return h.m.clearCellSID(ctx, sid, h.key)
}

func (h *Handle[T]) encode(v T) (cellRaw, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("session: cell %q: encode: %w", h.key, err)
	}
	return raw, nil
}

func (h *Handle[T]) decode(raw cellRaw) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		var zero T
		return zero, fmt.Errorf("session: cell %q: decode: %w", h.key, err)
	}
	return v, nil
}

// decodePrev maps an absent Update base to T's zero value.
func (h *Handle[T]) decodePrev(prev cellRaw) (T, error) {
	if prev == nil {
		var zero T
		return zero, nil
	}
	return h.decode(prev)
}
