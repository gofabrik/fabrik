package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Config controls manager-wide defaults. Only the store (passed to
// [New]) is required; every field here has a default.
type Config struct {
	// Logger is used for worker/scheduler diagnostics and handler
	// loggers. Defaults to slog.Default().
	Logger *slog.Logger
	// DefaultQueue is assigned when an enqueue names no queue. Defaults
	// to "default".
	DefaultQueue string
	// DefaultBackoff drives retries when a job has no serializable
	// per-job override. May be any [Backoff]. Defaults to [DefaultBackoff].
	DefaultBackoff Backoff
	// DefaultMaxAttempts is the retry cap when an enqueue names none.
	// Defaults to 25.
	DefaultMaxAttempts int
	// Hooks attach observability. Nil fields are skipped.
	Hooks Hooks
	// Location is the wall clock cron schedules fire on. Defaults to UTC.
	Location *time.Location
	// SchedulerGroup scopes schedule reconciliation. Defaults to "".
	SchedulerGroup string
	// SchedulerInterval is the tick cadence of [Manager.StartScheduler].
	// Defaults to 1s.
	SchedulerInterval time.Duration
	// StoreTimeout caps each runtime-driven store call (claim,
	// heartbeat, complete, sweep, fire). The zero value uses the 30s
	// default; set a negative value to disable it.
	StoreTimeout time.Duration
	// Now is the clock, overridable in tests. Defaults to time.Now.
	Now func() time.Time
}

// Manager is the entry point: register handlers, enqueue and publish
// work, inspect state, drive operator actions. Workers ([Worker]) are
// constructed separately and bound to a manager.
type Manager struct {
	store  Store
	config Config

	mu           sync.RWMutex
	kindByType   map[reflect.Type]string
	typeByKind   map[string]reflect.Type
	handlers     map[HandlerKey]handlerEntry
	kindHandlers map[string][]string // kind -> handler-ids, registration order

	schedMu  sync.Mutex
	declared map[string]ScheduleRow // schedule name -> declaration (this group)
}

// handlerEntry is a type-erased registered handler.
type handlerEntry struct {
	decode func([]byte) (any, error)
	invoke func(Context, any) error
}

// New constructs a manager bound to a store.
func New(s Store, cfg Config) (*Manager, error) {
	if s == nil {
		return nil, fmt.Errorf("jobs.New: store is nil")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.DefaultQueue == "" {
		cfg.DefaultQueue = "default"
	}
	if err := validIdent("DefaultQueue", cfg.DefaultQueue); err != nil {
		return nil, err
	}
	if cfg.DefaultBackoff == nil {
		cfg.DefaultBackoff = DefaultBackoff
	}
	if cfg.DefaultMaxAttempts < 0 {
		return nil, fmt.Errorf("jobs.New: DefaultMaxAttempts must be >= 0 (got %d)", cfg.DefaultMaxAttempts)
	}
	if cfg.DefaultMaxAttempts == 0 {
		cfg.DefaultMaxAttempts = 25
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	if cfg.StoreTimeout == 0 {
		cfg.StoreTimeout = 30 * time.Second
	} else if cfg.StoreTimeout < 0 {
		cfg.StoreTimeout = 0
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{
		store:        s,
		config:       cfg,
		kindByType:   make(map[reflect.Type]string),
		typeByKind:   make(map[string]reflect.Type),
		handlers:     make(map[HandlerKey]handlerEntry),
		kindHandlers: make(map[string][]string),
		declared:     make(map[string]ScheduleRow),
	}, nil
}

func (m *Manager) now() time.Time { return m.config.Now().UTC() }

// withStoreTimeout wraps a runtime-driven store call. Caller must call cancel.
func (m *Manager) withStoreTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if m.config.StoreTimeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, m.config.StoreTimeout)
}

// Register binds a message type to a stable persisted kind. Re-registering
// the identical (type, kind) pair is a no-op.
func Register[T any](m *Manager, kind string) error {
	if m == nil {
		return fmt.Errorf("jobs.Register: nil manager")
	}
	if err := validIdent("kind", kind); err != nil {
		return err
	}
	if strings.HasPrefix(kind, cronPrefix) {
		return fmt.Errorf("jobs.Register: kind %q uses the reserved %q prefix (used by RegisterCron)", kind, cronPrefix)
	}
	typ := reflect.TypeFor[T]()
	if typ.Kind() == reflect.Pointer {
		return fmt.Errorf("jobs.Register: message type %v must be a value, not a pointer; take T, not *T", typ)
	}
	if typ.Kind() != reflect.Struct {
		return fmt.Errorf("jobs.Register: message type %v must be a struct (messages are plain JSON structs), got %s", typ, typ.Kind())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.kindByType[typ]; ok {
		if existing == kind {
			return nil // idempotent
		}
		return fmt.Errorf("%w: %v is %q, not %q", ErrTypeAlreadyRegistered, typ, existing, kind)
	}
	if boundTo, ok := m.typeByKind[kind]; ok {
		return fmt.Errorf("%w: %q is bound to %v", ErrKindAlreadyRegistered, kind, boundTo)
	}
	m.kindByType[typ] = kind
	m.typeByKind[kind] = typ
	return nil
}

// On attaches a handler to a message type under a stable handler-id.
// One handler is a command; several handlers on one type are an event.
// The type must already be registered with [Register] (or [Handle]).
func On[T any](m *Manager, handlerID string, fn func(Context, T) error) error {
	if m == nil {
		return fmt.Errorf("jobs.On: nil manager")
	}
	if fn == nil {
		return fmt.Errorf("jobs.On: nil handler")
	}
	if err := validIdent("handler-id", handlerID); err != nil {
		return err
	}
	typ := reflect.TypeFor[T]()
	m.mu.Lock()
	defer m.mu.Unlock()
	kind, ok := m.kindByType[typ]
	if !ok {
		return fmt.Errorf("jobs.On: type %v is not registered; call Register[%v] first", typ, typ)
	}
	key := HandlerKey{Kind: kind, HandlerID: handlerID}
	if _, exists := m.handlers[key]; exists {
		return fmt.Errorf("%w: %s/%s", ErrHandlerAlreadyRegistered, kind, handlerID)
	}
	m.handlers[key] = handlerEntry{
		decode: func(payload []byte) (any, error) {
			var msg T
			if err := decodePayload(payload, &msg); err != nil {
				return nil, err
			}
			return msg, nil
		},
		invoke: func(ctx Context, msg any) error { return fn(ctx, msg.(T)) },
	}
	m.kindHandlers[kind] = append(m.kindHandlers[kind], handlerID)
	return nil
}

// Handle is the command shortcut: [Register] plus one [On] whose
// handler-id equals the kind.
func Handle[T any](m *Manager, kind string, fn func(Context, T) error) error {
	if err := Register[T](m, kind); err != nil {
		return err
	}
	return On[T](m, kind, fn)
}

// resolve maps a message value to its kind and the handler-ids
// registered for it (in registration order).
func (m *Manager) resolve(msg any) (kind string, ids []string, err error) {
	t := reflect.TypeOf(msg)
	if t == nil {
		return "", nil, fmt.Errorf("jobs: nil message")
	}
	// Runtime messages follow the same value-struct contract as Register.
	if t.Kind() == reflect.Pointer {
		return "", nil, fmt.Errorf("jobs: message %v must be a value, not a pointer", t)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	kind, ok := m.kindByType[t]
	if !ok {
		return "", nil, fmt.Errorf("%w: %v", ErrUnregistered, t)
	}
	ids = append([]string(nil), m.kindHandlers[kind]...)
	return kind, ids, nil
}

// handlerFor returns the registered handler for a key.
func (m *Manager) handlerFor(k HandlerKey) (handlerEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.handlers[k]
	return e, ok
}

// handlerSet returns this process's registered (kind, handler-id) set,
// for the claim filter.
func (m *Manager) handlerSet() map[HandlerKey]struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	set := make(map[HandlerKey]struct{}, len(m.handlers))
	for k := range m.handlers {
		set[k] = struct{}{}
	}
	return set
}

// Option configures a single enqueue or publish.
type Option func(*jobOpts)

type jobOpts struct {
	queue       string
	priority    int
	maxAttempts int
	backoff     Backoff
	timeout     time.Duration
	onTimeout   OnTimeout
	uniqueKey   string
	after       time.Duration
	at          time.Time
}

// After defers a job by a duration from now.
func After(d time.Duration) Option { return func(o *jobOpts) { o.after = d } }

// At defers a job until a specific time.
func At(t time.Time) Option { return func(o *jobOpts) { o.at = t } }

// Queue puts a job on a named queue.
func Queue(name string) Option { return func(o *jobOpts) { o.queue = name } }

// Priority sets the claim priority (higher is claimed first).
func Priority(p int) Option { return func(o *jobOpts) { o.priority = p } }

// MaxAttempts caps the number of runs.
func MaxAttempts(n int) Option { return func(o *jobOpts) { o.maxAttempts = n } }

// WithBackoff sets a per-job backoff override (must be serializable).
func WithBackoff(b Backoff) Option { return func(o *jobOpts) { o.backoff = b } }

// Timeout sets a per-attempt timeout.
func Timeout(d time.Duration) Option { return func(o *jobOpts) { o.timeout = d } }

// TimeoutAction sets what a timed-out attempt does.
func TimeoutAction(a OnTimeout) Option { return func(o *jobOpts) { o.onTimeout = a } }

// UniqueKey dedups against a live job of the same (kind, handler-id).
func UniqueKey(k string) Option { return func(o *jobOpts) { o.uniqueKey = k } }

func (m *Manager) buildJobs(kind string, ids []string, msg any, o *jobOpts, scheduledFor time.Time, schedSet bool) ([]Job, error) {
	payload, err := encodePayload(msg)
	if err != nil {
		return nil, err
	}
	spec, err := encodeBackoff(o.backoff)
	if err != nil {
		return nil, err
	}
	now := m.now()
	avail := now
	switch {
	case !o.at.IsZero():
		avail = o.at.UTC()
	case o.after > 0:
		avail = now.Add(o.after)
	}
	queue := o.queue
	if queue == "" {
		queue = m.config.DefaultQueue
	}
	if err := validIdent("queue", queue); err != nil {
		return nil, err
	}
	maxAtt := o.maxAttempts
	if maxAtt == 0 {
		maxAtt = m.config.DefaultMaxAttempts
	}
	if maxAtt < 0 {
		return nil, fmt.Errorf("jobs: MaxAttempts must be >= 0 (got %d)", maxAtt)
	}
	if o.uniqueKey != "" {
		// UniqueKey is a domain idempotency value, not a jobs identifier.
		if len(o.uniqueKey) > 255 {
			return nil, fmt.Errorf("jobs: UniqueKey exceeds 255 bytes")
		}
		if strings.IndexByte(o.uniqueKey, 0) >= 0 {
			return nil, fmt.Errorf("jobs: UniqueKey must not contain a null byte")
		}
	}
	if o.timeout < 0 {
		return nil, fmt.Errorf("jobs: Timeout must be >= 0 (got %v)", o.timeout)
	}
	timeoutMs := int64(o.timeout / time.Millisecond)
	if o.timeout > 0 && timeoutMs == 0 {
		timeoutMs = 1 // round a sub-millisecond timeout up so it still fires
	}
	out := make([]Job, 0, len(ids))
	for _, id := range ids {
		out = append(out, Job{
			Kind:            kind,
			HandlerID:       id,
			Payload:         payload,
			Queue:           queue,
			Priority:        o.priority,
			AvailableAt:     avail,
			MaxAttempts:     maxAtt,
			TimeoutMs:       timeoutMs,
			OnTimeout:       o.onTimeout,
			BackoffSpec:     spec,
			UniqueKey:       o.uniqueKey,
			ScheduledFor:    scheduledFor,
			ScheduledForSet: schedSet,
		})
	}
	return out, nil
}

func applyOpts(opts []Option) *jobOpts {
	o := &jobOpts{}
	for _, fn := range opts {
		fn(o)
	}
	return o
}

// Enqueue persists a command: the message's single handler. Returns the
// new job id. Errors if the type has zero handlers or more than one
// (use [Manager.Publish] for events). On a UniqueKey collision returns
// the existing id and a [*DuplicateError].
func (m *Manager) Enqueue(ctx context.Context, msg any, opts ...Option) (string, error) {
	kind, ids, err := m.resolve(msg)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("jobs.Enqueue: kind %q has no handler", kind)
	}
	if len(ids) > 1 {
		return "", fmt.Errorf("jobs.Enqueue: kind %q has %d handlers; use Publish for events", kind, len(ids))
	}
	rows, err := m.buildJobs(kind, ids, msg, applyOpts(opts), time.Time{}, false)
	if err != nil {
		return "", err
	}
	results, err := m.store.Insert(ctx, rows)
	if err != nil {
		return "", err
	}
	r := results[0]
	if r.Duplicate {
		return r.ID, &DuplicateError{ExistingID: r.ID, Kind: kind, HandlerID: ids[0], UniqueKey: rows[0].UniqueKey}
	}
	m.fireEnqueueHooks(ctx, results, kind, false, "")
	return r.ID, nil
}

// PublishResult is one handler's outcome from [Manager.Publish].
type PublishResult struct {
	HandlerID string
	JobID     string
	Duplicate bool
}

// Publish persists an event: one job per registered handler of the
// message type, in one atomic insert. A registered kind with no handlers
// is a no-op; an unregistered type is [ErrUnregistered]. Returns a
// per-handler result in registration order.
func (m *Manager) Publish(ctx context.Context, msg any, opts ...Option) ([]PublishResult, error) {
	kind, ids, err := m.resolve(msg)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := m.buildJobs(kind, ids, msg, applyOpts(opts), time.Time{}, false)
	if err != nil {
		return nil, err
	}
	results, err := m.store.Insert(ctx, rows)
	if err != nil {
		return nil, err
	}
	m.fireEnqueueHooks(ctx, results, kind, false, "")
	return toPublishResults(results), nil
}

// EnqueueTx enqueues a command inside the caller's transaction (the
// outbox pattern). Returns [ErrUnsupported] when the store lacks the
// capability. The job is visible to workers only after the caller
// commits.
func (m *Manager) EnqueueTx(ctx context.Context, tx *sql.Tx, msg any, opts ...Option) (string, error) {
	txs, ok := m.store.(TxEnqueuer)
	if !ok {
		return "", ErrUnsupported
	}
	kind, ids, err := m.resolve(msg)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("jobs.EnqueueTx: kind %q has no handler", kind)
	}
	if len(ids) > 1 {
		return "", fmt.Errorf("jobs.EnqueueTx: kind %q has %d handlers; use PublishTx", kind, len(ids))
	}
	rows, err := m.buildJobs(kind, ids, msg, applyOpts(opts), time.Time{}, false)
	if err != nil {
		return "", err
	}
	results, err := txs.InsertTx(ctx, tx, rows)
	if err != nil {
		return "", err
	}
	r := results[0]
	if r.Duplicate {
		return r.ID, &DuplicateError{ExistingID: r.ID, Kind: kind, HandlerID: ids[0], UniqueKey: rows[0].UniqueKey}
	}
	m.fireEnqueueHooks(ctx, results, kind, true, "")
	return r.ID, nil
}

// PublishTx publishes an event inside the caller's transaction.
func (m *Manager) PublishTx(ctx context.Context, tx *sql.Tx, msg any, opts ...Option) ([]PublishResult, error) {
	txs, ok := m.store.(TxEnqueuer)
	if !ok {
		return nil, ErrUnsupported
	}
	kind, ids, err := m.resolve(msg)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := m.buildJobs(kind, ids, msg, applyOpts(opts), time.Time{}, false)
	if err != nil {
		return nil, err
	}
	results, err := txs.InsertTx(ctx, tx, rows)
	if err != nil {
		return nil, err
	}
	m.fireEnqueueHooks(ctx, results, kind, true, "")
	return toPublishResults(results), nil
}

func toPublishResults(results []InsertResult) []PublishResult {
	out := make([]PublishResult, len(results))
	for i, r := range results {
		out[i] = PublishResult{HandlerID: r.HandlerID, JobID: r.ID, Duplicate: r.Duplicate}
	}
	return out
}

// fireEnqueueHooks emits OnEnqueue for newly inserted (non-duplicate)
// rows. Duplicate-skipped rows fire nothing.
func (m *Manager) fireEnqueueHooks(ctx context.Context, results []InsertResult, kind string, transactional bool, scheduleName string) {
	if m.config.Hooks.OnEnqueue == nil {
		return
	}
	for _, r := range results {
		if r.Duplicate {
			continue
		}
		m.safeHook("OnEnqueue", func() {
			m.config.Hooks.OnEnqueue(ctx, EnqueueEvent{
				JobID:         r.ID,
				Kind:          kind,
				HandlerID:     r.HandlerID,
				Transactional: transactional,
				ScheduleName:  scheduleName,
				ScheduledFor:  r.ScheduledFor,
			})
		})
	}
}

// safeHook isolates user hook panics.
func (m *Manager) safeHook(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			m.config.Logger.Error("jobs: hook panic recovered", "hook", name, "panic", r)
		}
	}()
	fn()
}
