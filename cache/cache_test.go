package cache_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/cache"
)

type rollup struct {
	Total int
}

func newCache[T any](t *testing.T, opts ...cache.Option) *cache.Cache[T] {
	t.Helper()
	c, err := cache.New[T](cache.NewMemoryStore(cache.MemoryOptions{}), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func frozen(at *time.Time) cache.Option {
	return cache.WithClock(func() time.Time { return *at })
}

func TestNewValidates(t *testing.T) {
	store := cache.NewMemoryStore(cache.MemoryOptions{})
	if _, err := cache.New[int](nil); err == nil {
		t.Fatal("nil store accepted")
	}
	for _, ns := range []string{"Bad", "sp ace", "under_score", "colon:", ""} {
		if _, err := cache.New[int](store, cache.WithNamespace(ns)); err == nil {
			t.Fatalf("namespace %q accepted", ns)
		}
	}
	if _, err := cache.New[int](store, cache.WithNamespace("greet-stats0")); err != nil {
		t.Fatalf("valid namespace rejected: %v", err)
	}
	if _, err := cache.New[int](store, cache.WithCodec(nil)); err == nil {
		t.Fatal("WithCodec(nil) accepted")
	}
	if _, err := cache.New[int](store, cache.WithClock(nil)); err == nil {
		t.Fatal("WithClock(nil) accepted")
	}
	if _, err := cache.New[int](store, cache.WithLogger(nil)); err == nil {
		t.Fatal("WithLogger(nil) accepted")
	}
}

func TestNamespacesIsolateSharedStore(t *testing.T) {
	ctx := context.Background()
	store := cache.NewMemoryStore(cache.MemoryOptions{})
	a, err := cache.New[string](store, cache.WithNamespace("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := cache.New[string](store, cache.WithNamespace("b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Set(ctx, "k", "va", 0); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := b.Get(ctx, "k"); ok {
		t.Fatal("namespace collision")
	}
	if v, ok, _ := a.Get(ctx, "k"); !ok || v != "va" {
		t.Fatalf("a.Get = %q %v", v, ok)
	}
}

func TestExpiryIsTheFrontsClock(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	c := newCache[string](t, frozen(&now))
	if err := c.Set(ctx, "k", "v", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(ctx, "k"); !ok {
		t.Fatal("fresh entry missing")
	}
	now = now.Add(10 * time.Second) // expiry instant itself is expired
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatal("entry served at its expiry instant")
	}
}

func TestZeroTTLNeverExpires(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0)
	c := newCache[string](t, frozen(&now))
	if err := c.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	now = now.Add(1000 * time.Hour)
	if _, ok, _ := c.Get(ctx, "k"); !ok {
		t.Fatal("no-expiry entry expired")
	}
}

func TestGetOrLoadSharesConcurrentLoad(t *testing.T) {
	ctx := context.Background()
	c := newCache[rollup](t)
	var loads atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad(ctx, "daily", time.Minute, func(context.Context) (rollup, error) {
				loads.Add(1)
				time.Sleep(10 * time.Millisecond)
				return rollup{Total: 42}, nil
			})
			if err != nil || v.Total != 42 {
				t.Errorf("GetOrLoad = %+v, %v", v, err)
			}
		}()
	}
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatalf("loads = %d, want 1", loads.Load())
	}
}

func TestGetOrLoadLoadErrorPropagatesUncached(t *testing.T) {
	ctx := context.Background()
	c := newCache[string](t)
	boom := errors.New("boom")
	if _, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) {
		return "", boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) {
		return "second", nil
	})
	if err != nil || v != "second" {
		t.Fatalf("after failed load: %q %v (error result cached?)", v, err)
	}
}

type faultStore struct {
	cache.Store
	getErr error
	setErr error
}

func (f *faultStore) Get(ctx context.Context, key string, now time.Time) (cache.Entry, bool, error) {
	if f.getErr != nil {
		return cache.Entry{}, false, f.getErr
	}
	return f.Store.Get(ctx, key, now)
}

func (f *faultStore) Set(ctx context.Context, key string, e cache.Entry) error {
	if f.setErr != nil {
		return f.setErr
	}
	return f.Store.Set(ctx, key, e)
}

func warnLogger(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestGetOrLoadFailOpenOnReadFault(t *testing.T) {
	ctx := context.Background()
	var logged strings.Builder
	fs := &faultStore{Store: cache.NewMemoryStore(cache.MemoryOptions{}), getErr: errors.New("backend down")}
	c, err := cache.New[string](fs, cache.WithLogger(warnLogger(&logged)))
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) {
		return "loaded", nil
	})
	if err != nil || v != "loaded" {
		t.Fatalf("read fault must degrade to a load: %q %v", v, err)
	}
	if !strings.Contains(logged.String(), "backend down") {
		t.Fatalf("read fault not logged: %q", logged.String())
	}
}

func TestGetOrLoadFailOpenOnWriteFault(t *testing.T) {
	ctx := context.Background()
	var logged strings.Builder
	fs := &faultStore{Store: cache.NewMemoryStore(cache.MemoryOptions{}), setErr: errors.New("disk full")}
	c, err := cache.New[string](fs, cache.WithLogger(warnLogger(&logged)))
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) {
		return "loaded", nil
	})
	if err != nil || v != "loaded" {
		t.Fatalf("write fault must not withhold the value: %q %v", v, err)
	}
	if !strings.Contains(logged.String(), "disk full") {
		t.Fatalf("write fault not logged: %q", logged.String())
	}
}

func TestGetOrLoadPreCanceledReturnsCtxErrWithoutLoading(t *testing.T) {
	c := newCache[string](t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loaded := false
	_, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) {
		loaded = true
		return "", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if loaded {
		t.Fatal("load ran under a pre-canceled ctx")
	}
}

func TestGetOrLoadCancellationIsNotLoggedAsFault(t *testing.T) {
	var logged strings.Builder
	store := cache.NewMemoryStore(cache.MemoryOptions{})
	c, err := cache.New[string](store, cache.WithLogger(warnLogger(&logged)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) { return "", nil }) //nolint:errcheck
	if logged.Len() != 0 {
		t.Fatalf("cancellation logged as cache fault: %q", logged.String())
	}
}

type countingStore struct {
	cache.Store
	sets atomic.Int32
}

func (s *countingStore) Set(ctx context.Context, key string, e cache.Entry) error {
	s.sets.Add(1)
	return s.Store.Set(ctx, key, e)
}

func TestGetOrLoadSkipsSetWhenCanceledDuringLoad(t *testing.T) {
	var logged strings.Builder
	cs := &countingStore{Store: cache.NewMemoryStore(cache.MemoryOptions{})}
	c, err := cache.New[string](cs, cache.WithLogger(warnLogger(&logged)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) {
		cancel()
		return "loaded", nil
	})
	if err != nil || v != "loaded" {
		t.Fatalf("loaded value withheld: %q %v", v, err)
	}
	if cs.sets.Load() != 0 {
		t.Fatal("Set ran after known cancellation")
	}
	if logged.Len() != 0 {
		t.Fatalf("post-load cancellation logged: %q", logged.String())
	}
}

// blockingStore parks Get until the context ends, then reports a miss.
type blockingStore struct {
	cache.Store
}

func (b *blockingStore) Get(ctx context.Context, key string, now time.Time) (cache.Entry, bool, error) {
	<-ctx.Done()
	return cache.Entry{}, false, nil
}

func TestGetOrLoadCanceledDuringReadDoesNotLoad(t *testing.T) {
	c, err := cache.New[string](&blockingStore{Store: cache.NewMemoryStore(cache.MemoryOptions{})})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(5 * time.Millisecond); cancel() }()
	loaded := false
	_, err = c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) {
		loaded = true
		return "", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if loaded {
		t.Fatal("load ran after cancellation during the store read")
	}
}

// cancelingSetStore cancels its context during Set and returns its error.
type cancelingSetStore struct {
	cache.Store
	cancel context.CancelFunc
}

func (s *cancelingSetStore) Set(ctx context.Context, key string, e cache.Entry) error {
	s.cancel()
	return ctx.Err()
}

func TestGetOrLoadSuppressesMidSetCancellation(t *testing.T) {
	var logged strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cs := &cancelingSetStore{Store: cache.NewMemoryStore(cache.MemoryOptions{}), cancel: cancel}
	c, err := cache.New[string](cs, cache.WithLogger(warnLogger(&logged)))
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) {
		return "loaded", nil
	})
	if err != nil || v != "loaded" {
		t.Fatalf("loaded value withheld on mid-Set cancellation: %q %v", v, err)
	}
	if logged.Len() != 0 {
		t.Fatalf("mid-Set cancellation logged as cache fault: %q", logged.String())
	}
}

func TestDeleteDuringLoadPreventsRepublish(t *testing.T) {
	ctx := context.Background()
	cs := &countingStore{Store: cache.NewMemoryStore(cache.MemoryOptions{})}
	c, err := cache.New[string](cs)
	if err != nil {
		t.Fatal(err)
	}
	inLoad := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) {
			close(inLoad)
			<-release
			return "stale", nil
		})
		if err != nil || v != "stale" {
			t.Errorf("loaded value withheld: %q %v", v, err)
		}
	}()
	<-inLoad
	// Deletion prevents the running load's stale value from being stored.
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if cs.sets.Load() != 0 {
		t.Fatal("delete during the load did not stop the store write")
	}
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatal("deleted key resurrected by a running load")
	}
}

// gateStore blocks Set to expose the publish window.
type gateStore struct {
	cache.Store
	entered chan struct{}
	release chan struct{}
	deletes atomic.Int32
}

func (g *gateStore) Set(ctx context.Context, key string, e cache.Entry) error {
	close(g.entered)
	<-g.release
	return g.Store.Set(ctx, key, e)
}

func (g *gateStore) Delete(ctx context.Context, key string) error {
	g.deletes.Add(1)
	return g.Store.Delete(ctx, key)
}

func TestDeleteDuringAdmittedWriteStillWins(t *testing.T) {
	ctx := context.Background()
	gs := &gateStore{
		Store:   cache.NewMemoryStore(cache.MemoryOptions{}),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	c, err := cache.New[string](gs)
	if err != nil {
		t.Fatal(err)
	}
	loaded := make(chan struct{})
	go func() {
		defer close(loaded)
		if v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) {
			return "stale", nil
		}); err != nil || v != "stale" {
			t.Errorf("GetOrLoad = %q %v", v, err)
		}
	}()
	<-gs.entered
	// Delete must not reach the store while a publish is in progress.
	shortCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := c.Delete(shortCtx, "k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded delete during admitted write: %v", err)
	}
	if gs.deletes.Load() != 0 {
		t.Fatal("expired delete still reached the store")
	}
	close(gs.release)
	<-loaded
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatal("delete lost to a store write already under way")
	}
}

func TestGetOrLoadServesFreshHitWithoutLoading(t *testing.T) {
	ctx := context.Background()
	c := newCache[string](t)
	if err := c.Set(ctx, "k", "cached", time.Minute); err != nil {
		t.Fatal(err)
	}
	v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) (string, error) {
		t.Fatal("load ran despite a fresh hit")
		return "", nil
	})
	if err != nil || v != "cached" {
		t.Fatalf("hit = %q %v", v, err)
	}
}

func TestDecodeFailureIsAnError(t *testing.T) {
	ctx := context.Background()
	store := cache.NewMemoryStore(cache.MemoryOptions{})
	if err := store.Set(ctx, "k", cache.Entry{Value: []byte("not json")}); err != nil {
		t.Fatal(err)
	}
	c, err := cache.New[rollup](store)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Get(ctx, "k"); err == nil {
		t.Fatal("corrupt entry decoded")
	}
}

func TestEncodeFailureStoresNothing(t *testing.T) {
	ctx := context.Background()
	cs := &countingStore{Store: cache.NewMemoryStore(cache.MemoryOptions{})}
	c, err := cache.New[func()](cs) // func values do not marshal to JSON
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, "k", func() {}, 0); err == nil {
		t.Fatal("unencodable value accepted")
	}
	if cs.sets.Load() != 0 {
		t.Fatal("store written despite encode failure")
	}
}

func TestDeleteRemoves(t *testing.T) {
	ctx := context.Background()
	c := newCache[string](t)
	if err := c.Set(ctx, "k", "v", 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Fatal("deleted key present")
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}
