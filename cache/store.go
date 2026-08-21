package cache

import (
	"context"
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
