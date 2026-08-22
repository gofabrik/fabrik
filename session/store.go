package session

import (
	"context"
	"time"
)

// Record is the unit a [Store] persists. Payload is opaque to stores.
//
// Version is the CAS token. A Save with Version 0 inserts and
// conflicts if the SID already exists; a Save with a nonzero version
// CASes against the stored version, and a missing record counts as a
// conflict (a revoked session stays revoked). A successful Save
// returns the record with the version incremented.
//
// AbsoluteExpiry is a hard deadline; SID rotation carries it
// unchanged. IdleExpiry slides forward as the session is used.
type Record struct {
	SID            string
	Version        uint64
	UserID         string
	AbsoluteExpiry time.Time
	IdleExpiry     time.Time
	Payload        []byte
}

// Store is the persistence contract for session records.
//
// Load returns an error wrapping [ErrNotFound] for missing records
// and for records past their expiry; pruning is the store's
// responsibility and is best-effort. Save follows the CAS contract on
// [Record]. Delete is idempotent: deleting a missing SID succeeds.
//
// Stores must copy Payload bytes on Save and Load.
//
// Error text returned from any method must not include the SID, the
// user ID, or any other caller-supplied identifier.
type Store interface {
	Load(ctx context.Context, sid string) (Record, error)
	Save(ctx context.Context, rec Record) (Record, error)
	Delete(ctx context.Context, sid string) error
}

// TTLBumper extends idle expiry without rewriting payload.
type TTLBumper interface {
	BumpTTL(ctx context.Context, sid string, until time.Time) error
}

// UserIndexer is implemented by stores that maintain a secondary
// index from user ID to session IDs. Required for
// [Manager.ListByUser] and [Manager.RevokeByUser].
//
// Implementations keep the index current on Save and Delete.
// ListByUser returns only live sessions; RevokeByUser deletes every
// matching row including expired ones.
type UserIndexer interface {
	ListByUser(ctx context.Context, userID string) ([]string, error)
	RevokeByUser(ctx context.Context, userID string, except ...string) (int, error)
}

// Scanner iterates live sessions.
type Scanner interface {
	Scan(ctx context.Context, fn func(sid string) bool) error
}

// Sweeper bulk-deletes expired records and returns the number removed.
type Sweeper interface {
	Sweep(ctx context.Context) (int, error)
}
