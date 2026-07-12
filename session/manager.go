package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"
)

// core is the non-generic session controller.
type core struct {
	cfg        Config
	now        func() time.Time
	newSID     func() (string, error)
	maxRetries int
	ctxKey     *managerKey

	regMu    sync.Mutex
	registry map[string]reflect.Type // cell key -> registered type
}

// managerKey is per-core, so managers do not share request state.
type managerKey struct{ _ byte }

// newCore validates cfg and builds the engine.
func newCore(cfg Config) (*core, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	m := &core{
		cfg:        cfg,
		now:        cfg.Now,
		newSID:     cfg.NewSID,
		maxRetries: cfg.MaxRetries,
		ctxKey:     &managerKey{},
		registry:   make(map[string]reflect.Type),
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

// register adds one (key, type) pair. Repeat registration is idempotent.
func (m *core) register(key string, t reflect.Type) error {
	m.regMu.Lock()
	defer m.regMu.Unlock()
	if prev, ok := m.registry[key]; ok {
		if prev != t {
			return fmt.Errorf("session: cell key %q is already registered with type %s (this registration: %s)",
				key, typeLabel(prev), typeLabel(t))
		}
		return nil
	}
	m.registry[key] = t
	return nil
}

// mintSID rejects empty generator output before it reaches the Store.
func (m *core) mintSID() (string, error) {
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
func (m *core) stateFromCtx(ctx context.Context, op string) (*state, error) {
	st, ok := ctx.Value(m.ctxKey).(*state)
	if !ok {
		return nil, fmt.Errorf("session.%s: %w", op, ErrNoSession)
	}
	return st, nil
}

// ensureLoaded loads on first session API call. Callers hold st.mu.
func (m *core) ensureLoaded(ctx context.Context, st *state) error {
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

// SID returns the SID the request arrived with.
func (m *core) SID(ctx context.Context) (string, error) {
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
func (m *core) UserID(ctx context.Context) (string, error) {
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

// Renew stages SID rotation without extending absolute expiry.
func (m *core) Renew(ctx context.Context) error {
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
func (m *core) Promote(ctx context.Context, userID string) error {
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
func (m *core) Destroy(ctx context.Context) error {
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

// DestroySID revokes one session out of band. Missing sessions succeed.
func (m *core) DestroySID(ctx context.Context, sid string) error {
	return m.cfg.Store.Delete(ctx, sid)
}

// ListForUser returns live SIDs for userID.
func (m *core) ListForUser(ctx context.Context, userID string) ([]string, error) {
	idx, ok := m.cfg.Store.(UserIndexer)
	if !ok {
		return nil, fmt.Errorf("session.ListForUser: %w", ErrCapabilityMissing)
	}
	return idx.ListByUser(ctx, userID)
}

// RevokeAllForUser deletes live sessions for userID except listed SIDs.
func (m *core) RevokeAllForUser(ctx context.Context, userID string, except ...string) (int, error) {
	idx, ok := m.cfg.Store.(UserIndexer)
	if !ok {
		return 0, fmt.Errorf("session.RevokeAllForUser: %w", ErrCapabilityMissing)
	}
	return idx.RevokeByUser(ctx, userID, except...)
}
