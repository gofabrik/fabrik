package ratelimit

import (
	"context"
	"time"
)

// Store atomically persists opaque int64 values with absolute expiry and must
// be safe for concurrent use; implementations use only caller-supplied times,
// treating entries expiring at or before now as absent to Get, replaceable by
// SetIfAbsent, and ineligible for CompareAndSwap.
type Store interface {
	Get(ctx context.Context, key string, now time.Time) (value int64, exists bool, err error)
	SetIfAbsent(ctx context.Context, key string, value int64, now, expiresAt time.Time) (ok bool, err error)
	CompareAndSwap(ctx context.Context, key string, old, new int64, now, expiresAt time.Time) (ok bool, err error)
}
