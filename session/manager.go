package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// Manager persists an application's session data per visitor. The first typed
// app-data accessor pins its struct type, and app data uses encoding/json.
// Manager also implements [Registry] for library-owned session cells.
type Manager struct {
	cfg        Config
	now        func() time.Time
	newSID     func() (string, error)
	maxRetries int
	ctxKey     *managerKey

	regMu sync.Mutex
	cells map[string]reflect.Type

	// appT caches the pinned app-cell type without taking regMu.
	appT atomic.Pointer[reflect.Type]
}

// managerKey prevents managers from sharing request state.
type managerKey struct{ _ byte }

// New validates all configuration fields and returns a Manager; the first typed
// app-data accessor pins its struct type.
func New(cfg Config) (*Manager, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	m := &Manager{
		cfg:        cfg,
		now:        cfg.Now,
		newSID:     cfg.NewSID,
		maxRetries: cfg.MaxRetries,
		ctxKey:     &managerKey{},
		cells:      make(map[string]reflect.Type),
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.newSID == nil {
		m.newSID = generateSID
	}
	switch {
	case m.maxRetries < 0:
		m.maxRetries = 0
	case m.maxRetries == 0:
		m.maxRetries = defaultMaxRetries
	}
	return m, nil
}

// registry seals [Registry] to Manager values.
func (m *Manager) registry() *Manager {
	if m == nil {
		return nil
	}
	return m
}

// register adds one (key, type) pair. Repeat registration is idempotent.
func (m *Manager) register(key string, t reflect.Type) error {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	if prev, ok := m.cells[key]; ok {
		if prev != t {
			return fmt.Errorf("session: cell key %q is already registered with type %s (this registration: %s)",
				key, typeLabel(prev), typeLabel(t))
		}
		return nil
	}
	m.cells[key] = t
	return nil
}

// mintSID rejects empty generator output before it reaches the Store.
func (m *Manager) mintSID() (string, error) {
	sid, err := m.newSID()
	if err != nil {
		return "", fmt.Errorf("session: generate sid: %w", err)
	}
	if sid == "" {
		return "", errors.New("session: generate sid: generator returned an empty SID")
	}
	return sid, nil
}

// state is the per-request bookkeeping [Manager.Middleware] attaches
// to the context. All fields are guarded by mu.
type state struct {
	mu sync.Mutex

	// Transport facts.
	arrivedSID string
	staleToken bool

	// Loaded record.
	loaded bool
	exists bool
	record Record

	// Decoded cell map of the established record.
	cells        map[string]cellRaw
	cellsDecoded bool
	envErr       error

	// Staged request state. A nil staged value is a tombstone.
	staged    map[string][]byte
	stagedSeq map[string]int

	// Lifecycle staging.
	destroyed    bool
	destroyedSID string
	renew        bool
	promote      bool
	promotedID   string

	// tokenNeeded asks commit to re-emit the refreshed token.
	tokenNeeded bool

	// responseStarted closes the staged path.
	responseStarted bool
}

// cellRaw is one stored cell's raw bytes.
type cellRaw = []byte

// stateFromCtx retrieves the per-request state attached by middleware.
func (m *Manager) stateFromCtx(ctx context.Context, op string) (*state, error) {
	st, ok := ctx.Value(m.ctxKey).(*state)
	if !ok {
		return nil, fmt.Errorf("session.%s: %w", op, ErrNoSession)
	}
	return st, nil
}

// ensureLoaded loads on first session API call. Callers hold st.mu.
func (m *Manager) ensureLoaded(ctx context.Context, st *state) error {
	if st.loaded {
		return nil
	}
	if st.arrivedSID == "" {
		st.loaded = true
		return nil
	}
	rec, err := m.cfg.Store.Load(ctx, st.arrivedSID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			st.staleToken = true
			st.arrivedSID = ""
			st.loaded = true
			return nil
		}
		return err
	}
	st.record = rec
	st.exists = true
	st.loaded = true
	return nil
}

// decodeCells decodes the established record's payload envelope once.
// Callers hold st.mu.
func (st *state) decodeCells() error {
	if st.cellsDecoded {
		return st.envErr
	}
	st.cellsDecoded = true
	if !st.exists || len(st.record.Payload) == 0 {
		st.cells = map[string]cellRaw{}
		return nil
	}
	cells, err := decodeEnvelope(st.record.Payload)
	if err != nil {
		st.envErr = err
		return err
	}
	st.cells = cells
	return nil
}

// pendingMint reports whether commit should mint a session.
func (st *state) pendingMint() bool {
	if st.promote {
		return true
	}
	for _, v := range st.staged {
		if v != nil {
			return true
		}
	}
	return false
}

// Has reports whether app session data exists, including staged writes.
func (m *Manager) Has(ctx context.Context) (bool, error) { return m.cellHas(ctx, appKey) }

// Clear stages removal of the app data without ending the session.
func (m *Manager) Clear(ctx context.Context) error { return m.cellClear(ctx, appKey) }

// SID returns the session ID the request arrived with.
func (m *Manager) SID(ctx context.Context) (string, error) {
	st, err := m.stateFromCtx(ctx, "SID")
	if err != nil {
		return "", err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := m.ensureLoaded(ctx, st); err != nil {
		return "", err
	}
	return st.arrivedSID, nil
}

// UserID returns the request-current session user ID.
func (m *Manager) UserID(ctx context.Context) (string, error) {
	st, err := m.stateFromCtx(ctx, "UserID")
	if err != nil {
		return "", err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := m.ensureLoaded(ctx, st); err != nil {
		return "", err
	}
	if st.promote {
		return st.promotedID, nil
	}
	if st.exists {
		return st.record.UserID, nil
	}
	return "", nil
}

// Renew stages a SID rotation without extending absolute expiry.
func (m *Manager) Renew(ctx context.Context) error {
	st, err := m.stateFromCtx(ctx, "Renew")
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.responseStarted {
		return fmt.Errorf("session.Renew: %w", ErrAlreadyCommitted)
	}
	if err := m.ensureLoaded(ctx, st); err != nil {
		return err
	}
	if !st.exists && !st.pendingMint() {
		return fmt.Errorf("session.Renew: %w", ErrNotFound)
	}
	st.renew = true
	return nil
}

// Promote stages login and rotates the SID, even for the same userID.
func (m *Manager) Promote(ctx context.Context, userID string) error {
	st, err := m.stateFromCtx(ctx, "Promote")
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.responseStarted {
		return fmt.Errorf("session.Promote: %w", ErrAlreadyCommitted)
	}
	if err := m.ensureLoaded(ctx, st); err != nil {
		return err
	}
	st.promote = true
	st.promotedID = userID
	if st.exists {
		st.renew = true
	}
	return nil
}

// Destroy stages deletion and leaves the request sessionless.
func (m *Manager) Destroy(ctx context.Context) error {
	st, err := m.stateFromCtx(ctx, "Destroy")
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.responseStarted {
		return fmt.Errorf("session.Destroy: %w", ErrAlreadyCommitted)
	}
	if err := m.ensureLoaded(ctx, st); err != nil {
		return err
	}
	if st.exists {
		st.destroyed = true
		st.destroyedSID = st.record.SID
	}
	st.exists = false
	st.record = Record{}
	st.cells = nil
	st.cellsDecoded = false
	st.envErr = nil
	st.staged = nil
	st.renew = false
	st.promote = false
	st.promotedID = ""
	st.tokenNeeded = false
	return nil
}

// ClearSID removes app data by SID without ending the session.
func (m *Manager) ClearSID(ctx context.Context, sid string) error {
	return m.clearCellSID(ctx, sid, appKey)
}

// DestroySID revokes one session. Revocation is idempotent.
func (m *Manager) DestroySID(ctx context.Context, sid string) error {
	return m.cfg.Store.Delete(ctx, sid)
}

// ListForUser returns the SIDs of every live session belonging to
// userID. Requires a store with the [UserIndexer] capability.
func (m *Manager) ListForUser(ctx context.Context, userID string) ([]string, error) {
	idx, ok := m.cfg.Store.(UserIndexer)
	if !ok {
		return nil, fmt.Errorf("session.ListForUser: %w", ErrCapabilityMissing)
	}
	return idx.ListByUser(ctx, userID)
}

// RevokeAllForUser deletes every live session for userID except the
// optional SIDs.
func (m *Manager) RevokeAllForUser(ctx context.Context, userID string, except ...string) (int, error) {
	idx, ok := m.cfg.Store.(UserIndexer)
	if !ok {
		return 0, fmt.Errorf("session.RevokeAllForUser: %w", ErrCapabilityMissing)
	}
	return idx.RevokeByUser(ctx, userID, except...)
}
