package password

import (
	"errors"
	"log/slog"

	"golang.org/x/crypto/bcrypt"
)

// Hasher hashes and verifies passwords. The default [BcryptHasher]
// uses bcrypt cost 12, the standard CPU-time balance between login
// latency and offline-cracking resistance for commodity hardware.
//
// Apps with stricter requirements plug in argon2id (e.g. via
// [github.com/alexedwards/argon2id]) or bump the bcrypt cost via
// [NewBcryptHasher].
type Hasher interface {
	// Hash returns a hash of plain suitable for persistence. The
	// format is the hasher's choice; Verify must accept the hashes
	// Hash produces.
	//
	// Implementations MUST accept an empty plain string: password.New
	// calls Hash("") once at boot to mint the timing-safety dummy
	// used on the unknown-email login path, and a Hash that rejects
	// empty input makes password.New return an error.
	Hash(plain string) ([]byte, error)

	// Verify reports whether plain matches the given hash. It
	// must take effectively constant time on a hash/plain pair -
	// bcrypt's CompareHashAndPassword already does this. Verify
	// returns false on any internal error (corrupt hash, wrong
	// algorithm, etc.) rather than a tri-state result; the caller
	// has no useful action on "couldn't verify".
	Verify(plain string, hash []byte) bool
}

// DefaultBcryptCost is the cost factor used by the package's
// default hasher. 12 takes roughly 250ms per Verify on commodity
// server hardware - a defensible default for interactive login
// (slow enough to dent offline cracking, fast enough not to be a
// noticeable form-submit lag).
const DefaultBcryptCost = 12

// BcryptHasher hashes with bcrypt at a configurable cost.
type BcryptHasher struct {
	Cost int // bcrypt.Cost; zero defaults to DefaultBcryptCost
}

// NewBcryptHasher returns a BcryptHasher with the given cost. A
// cost outside bcrypt's accepted range falls back to
// [DefaultBcryptCost].
func NewBcryptHasher(cost int) BcryptHasher {
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		cost = DefaultBcryptCost
	}
	return BcryptHasher{Cost: cost}
}

// Hash implements [Hasher].
func (h BcryptHasher) Hash(plain string) ([]byte, error) {
	cost := h.Cost
	if cost == 0 {
		cost = DefaultBcryptCost
	}
	return bcrypt.GenerateFromPassword([]byte(plain), cost)
}

// Verify implements [Hasher]. Returns false on any error.
//
// A bcrypt.ErrMismatchedHashAndPassword (wrong password) is the
// expected negative case and is returned silently. Any other error
// - ErrHashTooShort, ErrHashVersionTooNew, etc. - means the stored
// hash is corrupt or written by an algorithm this binary can't read.
// Those cases would otherwise silently 401 every login for the
// affected account with no diagnostic trail, so Verify logs them via
// [slog.Warn] before returning false.
//
// Apps that want to route or silence the warning should configure
// [slog.SetDefault] with a handler whose level filter or output
// destination suits them. The log line never contains the hash or
// the plaintext password.
func (h BcryptHasher) Verify(plain string, hash []byte) bool {
	err := bcrypt.CompareHashAndPassword(hash, []byte(plain))
	if err == nil {
		return true
	}
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false
	}
	slog.Warn("password: bcrypt verify failed with non-mismatch error",
		"err", err.Error(),
		"hint", "stored hash may be corrupt or use an unsupported algorithm",
	)
	return false
}
