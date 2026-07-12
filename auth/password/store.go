package password

import (
	"context"
	"errors"
)

// User is the password backend's view of a user row. Apps decide the
// shape of [User.ID] (UUIDv7 recommended) and supply the persistence
// via [Store]. There is no Name field - the backend treats display
// names as out of scope and never populates [auth.Identity.Name].
type User struct {
	ID       string // assigned by Store.Create; must be non-empty
	Email    string // canonical form; the emitted identity carries it
	PassHash []byte
}

// Store is the user table the password provider talks to. The
// provider holds the only references to PassHash and never leaks it
// to handlers or templates.
//
// Email normalization (case folding) is the Store's contract and
// must be consistent across LookupByEmail, Create, and the
// uniqueness constraint behind [ErrEmailTaken] - otherwise case
// variants register as separate accounts. Both methods return
// User.Email in canonical form and a non-empty User.ID; the provider
// validates the ID and treats an empty one as a store-contract
// error.
type Store interface {
	// LookupByEmail returns the user matching email or
	// [ErrUserNotFound]. Other errors (network, query) propagate.
	LookupByEmail(ctx context.Context, email string) (User, error)

	// Create inserts a new user, assigns a non-empty User.ID, and
	// returns the row. Returns [ErrEmailTaken] if the email exists.
	Create(ctx context.Context, email string, passHash []byte) (User, error)
}

// ErrUserNotFound is the sentinel a [Store] returns when a lookup
// finds no matching user. The provider translates it to a generic
// auth failure - never reveals to the client whether the email was
// registered.
var ErrUserNotFound = errors.New("password: user not found")

// ErrEmailTaken is the sentinel [Store.Create] returns when a user
// with the requested email already exists. The provider translates
// it to a generic register failure.
var ErrEmailTaken = errors.New("password: email already registered")
