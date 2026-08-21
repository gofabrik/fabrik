// Package session provides typed HTTP sessions.
//
//	type Session struct {
//		Name string
//	}
//
//	sessions, err := session.New(session.Config{
//		Store:          session.NewMemoryStore(session.MemoryOptions{}),
//		Token:          session.Cookie{Name: "session", HttpOnly: true},
//		AbsoluteExpiry: 24 * time.Hour,
//		IdleExpiry:     time.Hour,
//	})
//
//	handler := sessions.Middleware(mux)
//
//	s, err := sessions.Get[Session](r.Context())
//	s.Name = "alice"
//	err = sessions.Save(r.Context(), s)
//
// [Manager.Save] and [Manager.Clear] stage writes for the
// response-start commit. [Manager.Update] writes immediately with CAS
// retry. [Manager.Promote] is login and [Manager.Destroy] is logout.
//
// The package ships an in-memory store ([MemoryStore]), database-backed
// stores in the sqlite, postgres, and mysql subpackages, and cookie and
// bearer token transports ([Cookie], [Bearer], [Multi]). Stores declare
// optional capabilities via interfaces ([TTLBumper], [UserIndexer],
// [Scanner], [Sweeper]); the storetest subpackage is the
// conformance suite every store implementation runs.
//
// # For libraries
//
// A reusable library that needs private session data calls [Use] with its
// cell name and payload type against the [Registry].
//
// The library is standalone: net/http and any mux, no framework
// required.
package session

import (
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
