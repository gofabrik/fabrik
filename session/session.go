// Package session provides typed HTTP sessions.
//
//	type Session struct {
//		Name string
//	}
//
//	sessions, err := session.New[Session](session.Config{
//		Store:          session.NewMemoryStore(),
//		Token:          session.Cookie{Name: "session", HttpOnly: true},
//		AbsoluteExpiry: 24 * time.Hour,
//		IdleExpiry:     time.Hour,
//	})
//
//	handler := sessions.Middleware(mux)
//
//	s, err := sessions.Get(r.Context())
//	s.Name = "alice"
//	err = sessions.Save(r.Context(), s)
//
// [Manager.Save] and [Manager.Clear] stage writes for the
// response-start commit. [Manager.Update] writes immediately with CAS
// retry. [Manager.Promote] is login and [Manager.Destroy] is logout.
//
// The package ships an in-memory store ([MemoryStore]), a SQLite
// store ([SQLiteStore]), and cookie and bearer token transports
// ([Cookie], [Bearer], [Multi]). Stores declare optional
// capabilities via interfaces ([TTLBumper], [UserIndexer],
// [Scanner], [Sweeper]); the storetest subpackage is the
// conformance suite every store implementation runs.
//
// # For libraries
//
// A reusable library that needs private session data declares a typed
// [Key] and registers it with [Use] against [Registry].
//
// The library is standalone: net/http and any mux, no framework
// required.
package session

import (
	"context"
	"errors"
	"net/http"
	"time"
)

var (
	// ErrNotFound is wrapped when a session ID does not resolve to a
	// live record.
	ErrNotFound = errors.New("session not found")

	// ErrVersionConflict is wrapped by stores when a Save's Version
	// does not match the stored record. The manager retries CAS
	// conflicts up to Config.MaxRetries before surfacing it.
	ErrVersionConflict = errors.New("session version conflict")

	// ErrCapabilityMissing is wrapped when the configured store lacks
	// a required capability.
	ErrCapabilityMissing = errors.New("store capability missing")

	// ErrNoSession is returned by request-scoped operations when no
	// session state is attached to the context.
	ErrNoSession = errors.New("no session attached to context")

	// ErrAlreadyCommitted is returned by staged mutators after the
	// response has started.
	ErrAlreadyCommitted = errors.New("session already committed")
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
// [Manager.ListForUser] and [Manager.RevokeAllForUser].
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

// TokenWriteOptions describes how a token transport emits a session ID.
type TokenWriteOptions struct {
	Expiry time.Time

	// Now is the manager's clock at commit time. Zero means the wall
	// clock.
	Now time.Time
}

// Token is the request-side and response-side transport for a
// session ID. The package ships [Cookie] (browser), [Bearer]
// (header), and [Multi] (compose two or more).
type Token interface {
	Read(*http.Request) (sid string, ok bool)
	Write(w http.ResponseWriter, sid string, opts TokenWriteOptions)
	Clear(w http.ResponseWriter)
}
