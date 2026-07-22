// Package ratelimit provides keyed GCRA rate limiting with exact retry timing,
// future-slot reservations, and pluggable atomic stores that use the limiter's
// clock.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrInvalidLimit reports a malformed Limit.
var ErrInvalidLimit = errors.New("ratelimit: invalid limit")

// Limit permits Rate events per Period and Burst events at once; a zero Burst
// defaults to Rate.
type Limit struct {
	Rate   int
	Period time.Duration
	Burst  int
}

// PerSecond returns a Limit of n events per second.
func PerSecond(n int) Limit { return Limit{Rate: n, Period: time.Second} }

// PerMinute returns a Limit of n events per minute.
func PerMinute(n int) Limit { return Limit{Rate: n, Period: time.Minute} }

// PerHour returns a Limit of n events per hour.
func PerHour(n int) Limit { return Limit{Rate: n, Period: time.Hour} }

// WithBurst returns a copy of l with Burst set.
func (l Limit) WithBurst(b int) Limit { l.Burst = b; return l }

func (l Limit) capacity() int {
	if l.Burst > 0 {
		return l.Burst
	}
	return l.Rate
}

func (l Limit) emission() time.Duration { return l.Period / time.Duration(l.Rate) }

// Validate reports the first problem that makes the limit unusable.
func (l Limit) Validate() error {
	if l.Rate <= 0 {
		return fmt.Errorf("%w: Rate must be > 0", ErrInvalidLimit)
	}
	if l.Period <= 0 {
		return fmt.Errorf("%w: Period must be > 0", ErrInvalidLimit)
	}
	if l.Burst < 0 {
		return fmt.Errorf("%w: Burst must be >= 0", ErrInvalidLimit)
	}
	if l.emission() <= 0 {
		return fmt.Errorf("%w: Rate %d too high for Period %s (interval rounds to zero)", ErrInvalidLimit, l.Rate, l.Period)
	}
	if l.emission() > time.Duration(math.MaxInt64)/time.Duration(l.capacity()) {
		return fmt.Errorf("%w: Burst %d too large for Period %s (burst window overflows)", ErrInvalidLimit, l.capacity(), l.Period)
	}
	return nil
}

// Result reports an admission outcome; Limit and exact Remaining count unit
// events against burst capacity.
type Result struct {
	Allowed    bool
	Limit      int
	Remaining  int
	RetryAfter time.Duration // zero when Allowed
	ResetAfter time.Duration
}

// Limiter applies one Limit across keys and is safe for concurrent use.
type Limiter struct {
	limit   Limit
	store   Store
	now     func() time.Time
	prefix  string
	nsSet   bool
	horizon time.Duration
}

// DefaultReservationHorizon caps how far ahead Reserve may book a slot
// unless WithReservationHorizon overrides it.
const DefaultReservationHorizon = 24 * time.Hour

// Option configures a Limiter.
type Option func(*Limiter)

// WithClock injects the time source; defaults to time.Now.
func WithClock(now func() time.Time) Option { return func(l *Limiter) { l.now = now } }

// WithNamespace prefixes keys for store sharing; namespaces may contain
// lowercase letters, digits, and hyphens.
func WithNamespace(ns string) Option {
	return func(l *Limiter) { l.prefix, l.nsSet = ns, true }
}

// WithReservationHorizon overrides [DefaultReservationHorizon].
func WithReservationHorizon(d time.Duration) Option { return func(l *Limiter) { l.horizon = d } }

// New validates the limit and options and returns a Limiter.
func New(limit Limit, store Store, opts ...Option) (*Limiter, error) {
	if err := limit.Validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("ratelimit: nil store")
	}
	l := &Limiter{limit: limit, store: store, now: time.Now, horizon: DefaultReservationHorizon}
	for _, o := range opts {
		o(l)
	}
	if l.now == nil {
		return nil, fmt.Errorf("ratelimit: nil clock")
	}
	if l.horizon <= 0 {
		return nil, fmt.Errorf("ratelimit: reservation horizon must be > 0")
	}
	if l.nsSet {
		if err := validNamespace(l.prefix); err != nil {
			return nil, err
		}
		l.prefix += ":"
	}
	return l, nil
}

func validNamespace(ns string) error {
	if ns == "" {
		return fmt.Errorf("ratelimit: namespace must not be empty")
	}
	for i := 0; i < len(ns); i++ {
		b := ns[i]
		if b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '-' {
			continue
		}
		return fmt.Errorf("ratelimit: namespace %q must match [a-z0-9-]+", ns)
	}
	return nil
}

// Allow reports whether one event for key fits the limit right now.
func (l *Limiter) Allow(ctx context.Context, key string) (Result, error) {
	return l.AllowN(ctx, key, 1)
}

// AllowN admits n events at once; n must be between 1 and the burst capacity.
func (l *Limiter) AllowN(ctx context.Context, key string, n int) (Result, error) {
	res, _, err := l.admit(ctx, key, n, false)
	return res, err
}

// Reservation is a consumed future slot; schedule its absolute ReadyAt value
// instead of recomputing a delay on another clock.
type Reservation struct {
	ReadyAt time.Time
}

// DelayFrom converts the reservation to a wait relative to t, clamped
// at zero.
func (r Reservation) DelayFrom(t time.Time) time.Duration {
	if d := r.ReadyAt.Sub(t); d > 0 {
		return d
	}
	return 0
}

// Reserve consumes the next free slot for key; full buckets queue, while slots
// beyond the reservation horizon return an error without consuming capacity.
func (l *Limiter) Reserve(ctx context.Context, key string) (Reservation, error) {
	return l.ReserveN(ctx, key, 1)
}

// ReserveN consumes the next n slots; ReadyAt is the instant the last
// of them opens.
func (l *Limiter) ReserveN(ctx context.Context, key string, n int) (Reservation, error) {
	_, at, err := l.admit(ctx, key, n, true)
	if err != nil {
		return Reservation{}, err
	}
	return Reservation{ReadyAt: at}, nil
}

// Wait blocks until the reservation opens or ctx is done, computing the delay
// from the limiter's clock and sleeping on the process timer.
func (l *Limiter) Wait(ctx context.Context, r Reservation) error {
	delay := r.DelayFrom(l.now())
	if delay == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// admit retries CAS conflicts until success, cancellation, or a store error.
func (l *Limiter) admit(ctx context.Context, key string, n int, reserve bool) (Result, time.Time, error) {
	if n <= 0 {
		return Result{}, time.Time{}, fmt.Errorf("ratelimit: n must be > 0")
	}
	if n > l.limit.capacity() {
		return Result{}, time.Time{}, fmt.Errorf("ratelimit: n %d exceeds burst capacity %d", n, l.limit.capacity())
	}
	key = l.prefix + key
	emission := l.limit.emission()
	burst := time.Duration(l.limit.capacity()) * emission
	cost := time.Duration(n) * emission

	for {
		if err := ctx.Err(); err != nil {
			return Result{}, time.Time{}, err
		}
		now := l.now()
		stored, exists, err := l.store.Get(ctx, key, now)
		if err != nil {
			return Result{}, time.Time{}, err
		}
		tat := now
		if exists {
			if t := time.Unix(0, stored); t.After(now) {
				tat = t
			}
		}
		newTAT := tat.Add(cost)
		// Reject states that cannot round-trip through the store's Unix-nanosecond
		// format.
		if !time.Unix(0, newTAT.UnixNano()).Equal(newTAT) {
			return Result{}, time.Time{}, fmt.Errorf("ratelimit: state for %q is not representable in Unix nanoseconds", key)
		}
		allowAt := newTAT.Add(-burst)
		wait := allowAt.Sub(now)

		if !reserve && wait > 0 {
			return Result{
				Allowed:    false,
				Limit:      l.limit.capacity(),
				Remaining:  unitsRemaining(burst, tat.Sub(now), emission),
				RetryAfter: wait,
				ResetAfter: tat.Sub(now),
			}, time.Time{}, nil
		}
		if reserve && wait > l.horizon {
			return Result{}, time.Time{}, fmt.Errorf("ratelimit: reservation for %q opens in %s, beyond the %s horizon", key, wait, l.horizon)
		}

		var ok bool
		if exists {
			ok, err = l.store.CompareAndSwap(ctx, key, stored, newTAT.UnixNano(), now, newTAT)
		} else {
			ok, err = l.store.SetIfAbsent(ctx, key, newTAT.UnixNano(), now, newTAT)
		}
		if err != nil {
			return Result{}, time.Time{}, err
		}
		if !ok {
			continue
		}
		if wait < 0 {
			wait = 0
		}
		return Result{
			Allowed:    true,
			Limit:      l.limit.capacity(),
			Remaining:  unitsRemaining(burst, newTAT.Sub(now), emission),
			ResetAfter: newTAT.Sub(now),
		}, allowAt, nil
	}
}

func unitsRemaining(burst, fill, emission time.Duration) int {
	free := burst - fill
	if free < 0 {
		return 0
	}
	return int(free / emission)
}
