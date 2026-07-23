// Package cache caches computed values by key with TTL: a miss runs
// the caller's load function once and stores the result.
//
// Cached values must be recomputable because the cache may drop any
// entry through expiry, eviction, or restart.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"time"
)

// Entry is raw store data: encoded bytes plus absolute expiry. A zero
// Expires means no expiry.
type Entry struct {
	Value   []byte
	Expires time.Time
}

// Store holds entries by key and is safe for concurrent use. Get may
// use now for housekeeping but returns the addressed entry regardless
// of expiry; callers determine freshness. Stores may prune expired
// entries at any time. Values are copied in and out, and instants
// outside Entry's expiry domain return errors.
type Store interface {
	Get(ctx context.Context, key string, now time.Time) (Entry, bool, error)
	Set(ctx context.Context, key string, e Entry) error
	Delete(ctx context.Context, key string) error
}

// Sweeper optionally reclaims entries expired as of now and reports
// how many it removed.
type Sweeper interface {
	Sweep(ctx context.Context, now time.Time) (int, error)
}

// Codec marshals values to and from stored bytes.
type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}

type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error)      { return json.Marshal(v) }
func (jsonCodec) Unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// Cache is a concurrency-safe typed view over a Store with optional
// key namespacing and deduplicated loads.
type Cache[T any] struct {
	store     Store
	codec     Codec
	namespace string
	now       func() time.Time
	log       *slog.Logger
	loads     *loadGroup
}

type config struct {
	codec        Codec
	namespace    string
	namespaceSet bool
	now          func() time.Time
	log          *slog.Logger
	nilOption    string
}

// Option configures a Cache.
type Option func(*config)

// WithNamespace prefixes keys with namespace + ":" to isolate caches
// sharing a Store. The namespace must match [a-z0-9-]+.
func WithNamespace(namespace string) Option {
	return func(c *config) {
		c.namespace = namespace
		c.namespaceSet = true
	}
}

// WithCodec sets the value codec, which defaults to JSON.
func WithCodec(codec Codec) Option {
	return func(c *config) {
		if codec == nil {
			c.nilOption = "WithCodec"
			return
		}
		c.codec = codec
	}
}

// WithClock sets the time source, which defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(c *config) {
		if now == nil {
			c.nilOption = "WithClock"
			return
		}
		c.now = now
	}
}

// WithLogger sets the logger for ignored cache faults. It defaults to
// slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(c *config) {
		if log == nil {
			c.nilOption = "WithLogger"
			return
		}
		c.log = log
	}
}

var namespaceRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// New returns a typed cache over store.
func New[T any](store Store, opts ...Option) (*Cache[T], error) {
	if store == nil {
		return nil, errors.New("cache.New: store is required")
	}
	cfg := config{codec: jsonCodec{}, now: time.Now, log: slog.Default()}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.nilOption != "" {
		return nil, fmt.Errorf("cache.New: %s(nil)", cfg.nilOption)
	}
	if cfg.namespaceSet && !namespaceRE.MatchString(cfg.namespace) {
		return nil, fmt.Errorf("cache.New: namespace %q must match [a-z0-9-]+", cfg.namespace)
	}
	return &Cache[T]{
		store:     store,
		codec:     cfg.codec,
		namespace: cfg.namespace,
		now:       cfg.now,
		log:       cfg.log,
		loads:     newLoadGroup(),
	}, nil
}

func (c *Cache[T]) key(k string) string {
	if c.namespace == "" {
		return k
	}
	return c.namespace + ":" + k
}

func (c *Cache[T]) warn(op, key string, err error) {
	c.log.Warn("cache fault ignored", "op", op, "key", key, "error", err)
}

// lookup uses one clock sample for store housekeeping and freshness.
func (c *Cache[T]) lookup(ctx context.Context, key string) (T, bool, error) {
	var zero T
	now := c.now()
	e, ok, err := c.store.Get(ctx, c.key(key), now)
	if err != nil {
		return zero, false, fmt.Errorf("cache: get %q: %w", key, err)
	}
	if !ok || expired(e, now) {
		return zero, false, nil
	}
	var v T
	if err := c.codec.Unmarshal(e.Value, &v); err != nil {
		return zero, false, fmt.Errorf("cache: decode %q: %w", key, err)
	}
	return v, true, nil
}

// Get returns the fresh value for key or reports a miss.
func (c *Cache[T]) Get(ctx context.Context, key string) (T, bool, error) {
	return c.lookup(ctx, key)
}

// Set stores v under key; a ttl <= 0 means no expiry.
func (c *Cache[T]) Set(ctx context.Context, key string, v T, ttl time.Duration) error {
	data, err := c.codec.Marshal(v)
	if err != nil {
		return fmt.Errorf("cache: encode %q: %w", key, err)
	}
	var expires time.Time
	if ttl > 0 {
		expires = c.now().Add(ttl)
	}
	if err := c.store.Set(ctx, c.key(key), Entry{Value: data, Expires: expires}); err != nil {
		return fmt.Errorf("cache: set %q: %w", key, err)
	}
	return nil
}

// Delete removes key. A load already running for the key in this
// Cache instance will not store its result; if that load's store
// write has started, Delete waits for it up to ctx's deadline so the
// deletion wins. Other Cache instances and processes do not see the
// deletion signal; their loads age out by expiry.
func (c *Cache[T]) Delete(ctx context.Context, key string) error {
	if err := c.loads.invalidate(ctx, c.key(key)); err != nil {
		return fmt.Errorf("cache: delete %q: %w", key, err)
	}
	if err := c.store.Delete(ctx, c.key(key)); err != nil {
		return fmt.Errorf("cache: delete %q: %w", key, err)
	}
	return nil
}

// GetOrLoad returns the stored value when present and unexpired;
// otherwise it calls load once and stores the result for ttl.
//
// Concurrent callers for one key share that single load call. A
// caller whose ctx ends while waiting gets its own context error; if
// the caller running load is canceled, one of the waiting callers
// takes over and runs load itself. Load errors are not cached, and
// load panics propagate to every caller. Calling GetOrLoad for the
// same key from inside load deadlocks.
//
// A failed cache read is treated as a miss and a failed write after a
// successful load is ignored; both log at Warn and the value is
// returned either way. Caller cancellation returns ctx.Err() without
// loading; cancellation after a successful load skips the write but
// still returns the value.
func (c *Cache[T]) GetOrLoad(ctx context.Context, key string, ttl time.Duration, load func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if v, ok, err := c.lookupOrMiss(ctx, key); ok {
		return v, nil
	} else if err != nil {
		return zero, err
	}
	res, err := c.loads.Do(ctx, c.key(key), func(publish func(write func()) bool) (any, error) {
		if v, ok, err := c.lookupOrMiss(ctx, key); ok {
			return v, nil
		} else if err != nil {
			return nil, err
		}
		// The recheck may have blocked; cancellation stops new work.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		loaded, err := load(ctx)
		if err != nil {
			return nil, err
		}
		// Canceled or invalidated loads return their value without caching
		// it; publish serializes write admission with Delete.
		if ctx.Err() == nil {
			publish(func() {
				if err := c.Set(ctx, key, loaded, ttl); err != nil && !isCtxErr(ctx, err) {
					c.warn("set", key, err)
				}
			})
		}
		return loaded, nil
	})
	if err != nil {
		return zero, err
	}
	return res.(T), nil
}

// lookupOrMiss converts cache faults, but not caller cancellation, to misses.
func (c *Cache[T]) lookupOrMiss(ctx context.Context, key string) (T, bool, error) {
	var zero T
	v, ok, err := c.lookup(ctx, key)
	if err != nil {
		if isCtxErr(ctx, err) {
			return zero, false, ctx.Err()
		}
		c.warn("get", key, err)
		return zero, false, nil
	}
	return v, ok, nil
}

func isCtxErr(ctx context.Context, err error) bool {
	return ctx.Err() != nil && errors.Is(err, ctx.Err())
}

// minInstant and maxInstant bound the expiry domain: instants
// representable as int64 Unix nanoseconds.
var (
	minInstant = time.Unix(0, math.MinInt64)
	maxInstant = time.Unix(0, math.MaxInt64)
)

func checkExpiry(t time.Time) error {
	if t.IsZero() {
		return nil
	}
	return checkNow(t)
}

func checkNow(t time.Time) error {
	if t.Before(minInstant) || t.After(maxInstant) {
		return fmt.Errorf("instant %v outside the int64 unix-nano domain", t)
	}
	return nil
}
