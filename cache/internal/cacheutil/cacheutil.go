// Package cacheutil provides validation helpers for cache stores.
package cacheutil

import (
	"context"
	"fmt"
	"math"
	"time"
)

// MinInstant and MaxInstant bound the int64 Unix-nanosecond domain.
var (
	MinInstant = time.Unix(0, math.MinInt64)
	MaxInstant = time.Unix(0, math.MaxInt64)
)

// CheckNow returns an error when t is outside the int64 unix-nano domain.
func CheckNow(t time.Time) error {
	if t.Before(MinInstant) || t.After(MaxInstant) {
		return fmt.Errorf("instant %v outside the int64 unix-nano domain", t)
	}
	return nil
}

// CheckExpiry accepts zero as the no-expiry sentinel.
func CheckExpiry(t time.Time) error {
	if t.IsZero() {
		return nil
	}
	return CheckNow(t)
}

// OpCtx returns an error when ctx is already canceled.
func OpCtx(ctx context.Context, op, key string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cache: %s %q: %w", op, key, err)
	}
	return nil
}

// OpNow validates the context and current time for an operation.
func OpNow(ctx context.Context, op, key string, now time.Time) error {
	if err := OpCtx(ctx, op, key); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("cache: %s %q: zero instant", op, key)
	}
	if err := CheckNow(now); err != nil {
		return fmt.Errorf("cache: %s %q: %w", op, key, err)
	}
	return nil
}

// OpExpiry validates the context and expiry for an operation.
func OpExpiry(ctx context.Context, op, key string, expires time.Time) error {
	if err := OpCtx(ctx, op, key); err != nil {
		return err
	}
	if err := CheckExpiry(expires); err != nil {
		return fmt.Errorf("cache: %s %q: %w", op, key, err)
	}
	return nil
}
