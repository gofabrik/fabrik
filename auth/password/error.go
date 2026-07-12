package password

import "errors"

// Op names the handler stage a failure occurred in. Custom
// [Options.OnFailure] handlers branch on it via errors.As on
// [*Error], never on string prefixes.
type Op string

const (
	OpParse   Op = "parse"   // credential body did not parse
	OpLookup  Op = "lookup"  // Store.LookupByEmail failed (not ErrUserNotFound)
	OpHash    Op = "hash"    // Hasher.Hash failed
	OpCreate  Op = "create"  // Store.Create failed (not ErrEmailTaken)
	OpSession Op = "session" // the sink's Login failed during login/register
	OpLogout  Op = "logout"  // the sink's Logout failed
	OpHook    Op = "hook"    // a success/registered hook failed after auth succeeded
)

// Error wraps a handler-stage failure for [Options.OnFailure]
// handlers: the stage in Op, the cause in Err. Err is always
// non-nil - the provider never wraps nothing. Branch with errors.As
// on *Error for the stage and errors.Is through Unwrap for the
// sentinels.
type Error struct {
	Op  Op
	Err error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return string(e.Op) + ": <nil>"
	}
	return string(e.Op) + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// opError wraps err at stage op. Internal; the only constructor of
// [Error], so Err is non-nil by construction.
func opError(op Op, err error) *Error { return &Error{Op: op, Err: err} }

// IsOperational reports whether err is a backend/infrastructure
// failure (store outage, hashing, session write) rather than an
// expected credential, validation, or client outcome. Response
// routing (500 vs form/401) and operational logging both key off it,
// so the classification lives here once. A bare sentinel
// (ErrInvalidCredentials, ErrEmailTaken, a *PasswordTooShortError /
// *PasswordTooLongError) and the parse stage are not operational;
// every other wrapped stage is.
func IsOperational(err error) bool {
	var pe *Error
	if !errors.As(err, &pe) {
		return false
	}
	if errors.Is(err, ErrEmailTaken) { // OpCreate wrapping the taken sentinel is expected
		return false
	}
	return pe.Op != OpParse
}
