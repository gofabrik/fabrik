package ratelimit_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/ratelimit"
)

var base = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

func frozen() func() time.Time { return func() time.Time { return base } }

func newLimiter(t *testing.T, l ratelimit.Limit, opts ...ratelimit.Option) *ratelimit.Limiter {
	t.Helper()
	opts = append([]ratelimit.Option{ratelimit.WithClock(frozen())}, opts...)
	lim, err := ratelimit.New(l, ratelimit.NewMemoryStore(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return lim
}

func TestLimitValidate(t *testing.T) {
	valid := []ratelimit.Limit{
		ratelimit.PerSecond(1),
		ratelimit.PerMinute(100).WithBurst(20),
		ratelimit.PerHour(1),
	}
	for _, l := range valid {
		if err := l.Validate(); err != nil {
			t.Errorf("%+v: %v", l, err)
		}
	}
	invalid := []ratelimit.Limit{
		{},
		{Rate: -1, Period: time.Second},
		{Rate: 1, Period: -time.Second},
		{Rate: 1, Period: time.Second, Burst: -1},
		{Rate: 2_000_000_000, Period: time.Nanosecond},              // zero emission interval
		{Rate: 1, Period: time.Duration(1) << 62, Burst: 1_000_000}, // overflowing burst window
	}
	for _, l := range invalid {
		if err := l.Validate(); !errors.Is(err, ratelimit.ErrInvalidLimit) {
			t.Errorf("%+v: err = %v, want ErrInvalidLimit", l, err)
		}
	}
}

// spyStore verifies that rejected or canceled admissions do not write.
type spyStore struct {
	ratelimit.Store
	gets, writes int32
}

func newSpy() *spyStore { return &spyStore{Store: ratelimit.NewMemoryStore()} }

func (s *spyStore) Get(ctx context.Context, key string, now time.Time) (int64, bool, error) {
	atomic.AddInt32(&s.gets, 1)
	return s.Store.Get(ctx, key, now)
}

func (s *spyStore) SetIfAbsent(ctx context.Context, key string, value int64, now, expiresAt time.Time) (bool, error) {
	atomic.AddInt32(&s.writes, 1)
	return s.Store.SetIfAbsent(ctx, key, value, now, expiresAt)
}

func (s *spyStore) CompareAndSwap(ctx context.Context, key string, old, new int64, now, expiresAt time.Time) (bool, error) {
	atomic.AddInt32(&s.writes, 1)
	return s.Store.CompareAndSwap(ctx, key, old, new, now, expiresAt)
}

// losingStore forces CAS conflicts and signals after the first write attempt.
type losingStore struct {
	ratelimit.Store
	attempted chan struct{}
	once      sync.Once
}

func (s *losingStore) signal() {
	s.once.Do(func() { close(s.attempted) })
}

func (s *losingStore) SetIfAbsent(ctx context.Context, key string, value int64, now, expiresAt time.Time) (bool, error) {
	s.signal()
	return false, nil
}

func (s *losingStore) CompareAndSwap(ctx context.Context, key string, old, new int64, now, expiresAt time.Time) (bool, error) {
	s.signal()
	return false, nil
}

func TestNew_Validation(t *testing.T) {
	if _, err := ratelimit.New(ratelimit.PerSecond(1), nil); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := ratelimit.New(ratelimit.PerSecond(1), ratelimit.NewMemoryStore(), ratelimit.WithClock(nil)); err == nil {
		t.Error("nil clock accepted")
	}
	if _, err := ratelimit.New(ratelimit.Limit{}, ratelimit.NewMemoryStore()); err == nil {
		t.Error("invalid limit accepted")
	}
	if _, err := ratelimit.New(ratelimit.PerSecond(1), ratelimit.NewMemoryStore(), ratelimit.WithNamespace("Bad:Ns")); err == nil {
		t.Error("invalid namespace accepted")
	}
	if _, err := ratelimit.New(ratelimit.PerSecond(1), ratelimit.NewMemoryStore(), ratelimit.WithNamespace("good-ns2")); err != nil {
		t.Errorf("valid namespace rejected: %v", err)
	}
	if _, err := ratelimit.New(ratelimit.PerSecond(1), ratelimit.NewMemoryStore(), ratelimit.WithNamespace("")); err == nil {
		t.Error("explicitly empty namespace accepted")
	}
}

func TestAllow_Exactness(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerMinute(6).WithBurst(3))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		res, err := lim.Allow(ctx, "k")
		if err != nil || !res.Allowed {
			t.Fatalf("call %d: %+v err=%v", i+1, res, err)
		}
		if res.Limit != 3 {
			t.Errorf("Limit = %d, want burst capacity 3", res.Limit)
		}
		if res.Remaining != 2-i {
			t.Errorf("call %d Remaining = %d, want %d", i+1, res.Remaining, 2-i)
		}
	}
	res, err := lim.Allow(ctx, "k")
	if err != nil || res.Allowed {
		t.Fatalf("fourth call must deny: %+v err=%v", res, err)
	}
	if res.RetryAfter != 10*time.Second {
		t.Errorf("RetryAfter = %s, want exactly 10s (emission)", res.RetryAfter)
	}
}

func TestAllowN_WeightedDenialRemaining(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerMinute(5).WithBurst(5))
	ctx := context.Background()
	if _, err := lim.AllowN(ctx, "k", 2); err != nil {
		t.Fatal(err)
	}
	res, err := lim.AllowN(ctx, "k", 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.Allowed {
		t.Fatal("AllowN(4) with 3 units free must deny")
	}
	if res.Remaining != 3 {
		t.Errorf("denied AllowN Remaining = %d, want the exact 3 unit requests still admissible", res.Remaining)
	}
}

func TestAllowN_Bounds(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerSecond(2).WithBurst(2))
	ctx := context.Background()
	if _, err := lim.AllowN(ctx, "k", 0); err == nil {
		t.Error("n=0 accepted")
	}
	if _, err := lim.AllowN(ctx, "k", 3); err == nil {
		t.Error("n above capacity accepted; it can never fit")
	}
	if _, err := lim.ReserveN(ctx, "k", 3); err == nil {
		t.Error("ReserveN above capacity accepted")
	}
}

func TestReserve_SpacingAndHorizon(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerSecond(1).WithBurst(1))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		r, err := lim.Reserve(ctx, "k")
		if err != nil {
			t.Fatal(err)
		}
		want := base.Add(time.Duration(i) * time.Second)
		if !r.ReadyAt.Equal(want) {
			t.Errorf("reservation %d ReadyAt = %s, want %s", i, r.ReadyAt, want)
		}
	}

	capped := newLimiter(t, ratelimit.PerHour(1).WithBurst(1), ratelimit.WithReservationHorizon(90*time.Minute))
	if _, err := capped.Reserve(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := capped.Reserve(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	spy := newSpy()
	cappedSpy, err := ratelimit.New(ratelimit.PerHour(1).WithBurst(1), spy,
		ratelimit.WithClock(frozen()), ratelimit.WithReservationHorizon(90*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	cappedSpy.Reserve(ctx, "k")
	cappedSpy.Reserve(ctx, "k")
	writesBefore := atomic.LoadInt32(&spy.writes)
	if _, err := cappedSpy.Reserve(ctx, "k"); err == nil {
		t.Fatal("reservation beyond the horizon accepted")
	}
	if got := atomic.LoadInt32(&spy.writes); got != writesBefore {
		t.Fatalf("horizon rejection wrote to the store (%d -> %d writes); it must consume nothing", writesBefore, got)
	}
}

func TestReserve_DefaultHorizon(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerHour(1).WithBurst(1))
	ctx := context.Background()
	// A reservation exactly at the 24-hour horizon is valid.
	for i := 0; i < 25; i++ {
		if _, err := lim.Reserve(ctx, "k"); err != nil {
			t.Fatalf("reservation %d within the 24h default horizon: %v", i, err)
		}
	}
	if _, err := lim.Reserve(ctx, "k"); err == nil {
		t.Fatal("reservation past the 24h default horizon accepted")
	}
}

func TestWait(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerSecond(1).WithBurst(1))
	ctx := context.Background()
	r, err := lim.Reserve(ctx, "past")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := lim.Wait(ctx, r); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Error("Wait for a past-or-now ReadyAt must return immediately under an injected clock")
	}

	future, _ := lim.Reserve(ctx, "past")
	cctx, cancel := context.WithCancel(ctx)
	go cancel()
	if err := lim.Wait(cctx, future); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait under cancellation = %v, want context.Canceled", err)
	}
}

func TestAdmit_ContextCancellation(t *testing.T) {
	spy := newSpy()
	lim, err := ratelimit.New(ratelimit.PerSecond(1), spy, ratelimit.WithClock(frozen()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lim.Allow(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Allow with pre-canceled ctx = %v", err)
	}
	if _, err := lim.Reserve(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("Reserve with pre-canceled ctx = %v", err)
	}
	if gets, writes := atomic.LoadInt32(&spy.gets), atomic.LoadInt32(&spy.writes); gets != 0 || writes != 0 {
		t.Fatalf("pre-canceled admission touched the store (gets=%d writes=%d)", gets, writes)
	}

	// Cancel after the first write attempt to exercise mid-retry cancellation.
	ls := &losingStore{Store: ratelimit.NewMemoryStore(), attempted: make(chan struct{})}
	losing, err := ratelimit.New(ratelimit.PerSecond(1), ls, ratelimit.WithClock(frozen()))
	if err != nil {
		t.Fatal(err)
	}
	lctx, lcancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := losing.Allow(lctx, "k")
		done <- err
	}()
	<-ls.attempted
	lcancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("losing CAS loop returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("losing CAS loop did not end on cancellation")
	}
}

func TestNamespaceIsolation(t *testing.T) {
	store := ratelimit.NewMemoryStore()
	a, err := ratelimit.New(ratelimit.PerSecond(1).WithBurst(1), store, ratelimit.WithClock(frozen()), ratelimit.WithNamespace("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ratelimit.New(ratelimit.PerSecond(1).WithBurst(1), store, ratelimit.WithClock(frozen()), ratelimit.WithNamespace("b"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if res, _ := a.Allow(ctx, "k"); !res.Allowed {
		t.Fatal("a: first call denied")
	}
	if res, _ := b.Allow(ctx, "k"); !res.Allowed {
		t.Fatal("b: same key other namespace must be independent")
	}
	if res, _ := a.Allow(ctx, "k"); res.Allowed {
		t.Fatal("a: second call must deny")
	}
}

func TestOverflowBoundaryRejected(t *testing.T) {
	// The first admission crosses the Unix-nanosecond boundary.
	nearMax := time.Unix(0, math.MaxInt64).Add(-30 * time.Minute)
	ctx := context.Background()

	spy := newSpy()
	lim, err := ratelimit.New(ratelimit.PerHour(1).WithBurst(1), spy,
		ratelimit.WithClock(func() time.Time { return nearMax }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lim.Allow(ctx, "k"); err == nil || !strings.Contains(err.Error(), "representable") {
		t.Fatalf("Allow across the UnixNano boundary = %v, want representability rejection", err)
	}
	if _, err := lim.Reserve(ctx, "k"); err == nil || !strings.Contains(err.Error(), "representable") {
		t.Fatalf("Reserve across the UnixNano boundary = %v, want representability rejection", err)
	}
	if got := atomic.LoadInt32(&spy.writes); got != 0 {
		t.Fatalf("boundary rejection reached the store (%d writes); it must happen before any write", got)
	}

	// Denied admissions must also reject an unrepresentable next state.
	denySpy := newSpy()
	seedClock := time.Unix(0, math.MaxInt64).Add(-90 * time.Minute)
	denied, err := ratelimit.New(ratelimit.PerHour(1).WithBurst(1), denySpy,
		ratelimit.WithClock(func() time.Time { return seedClock }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := denied.Allow(ctx, "k"); err != nil {
		t.Fatalf("seed admission: %v", err)
	}
	writesAfterSeed := atomic.LoadInt32(&denySpy.writes)
	if _, err := denied.Allow(ctx, "k"); err == nil || !strings.Contains(err.Error(), "representable") {
		t.Fatalf("denied-path admission across the boundary = %v, want representability rejection", err)
	}
	if got := atomic.LoadInt32(&denySpy.writes); got != writesAfterSeed {
		t.Fatalf("denied-path boundary rejection wrote to the store (%d -> %d)", writesAfterSeed, got)
	}
}

func TestConcurrentAdmissionExact(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerMinute(60).WithBurst(10))
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := lim.Allow(context.Background(), "k")
			if err != nil {
				t.Error(err)
				return
			}
			if res.Allowed {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 10 {
		t.Fatalf("allowed = %d, want exactly the burst capacity", allowed)
	}
}

func TestConcurrentReservationSpacing(t *testing.T) {
	lim := newLimiter(t, ratelimit.PerSecond(1).WithBurst(1))
	const n = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[int64]int{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := lim.Reserve(context.Background(), "k")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			got[r.ReadyAt.UnixNano()]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		want := base.Add(time.Duration(i) * time.Second).UnixNano()
		if got[want] != 1 {
			t.Fatalf("slot %d claimed %d times, want exactly once", i, got[want])
		}
	}
}
