package password

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gofabrik/fabrik/auth"
)

var (
	// ErrInvalidCredentials reports invalid login input or credentials.
	ErrInvalidCredentials = errors.New("password: invalid credentials")

	// ErrNotFound is returned by stores for unknown emails.
	ErrNotFound = errors.New("password: credential not found")

	// ErrHashChanged reports that UpdateHash found a different current hash.
	ErrHashChanged = errors.New("password: hash changed since lookup")
)

// MaxEmailLen bounds email inputs before store or hashing work.
const MaxEmailLen = 254

// NormalizeEmail trims surrounding whitespace and lowercases an email.
// Account-keyed services must use the same normalization as their Store.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Credential contains an account's password hash and login claims.
type Credential struct {
	Hash   string
	Claims auth.ClaimSet
}

// Store looks up credentials by email. The store owns email
// normalization; Lookup returns an error wrapping ErrNotFound for
// unknown emails.
type Store interface {
	Lookup(ctx context.Context, email string) (Credential, error)
}

// Rehasher optionally upgrades stored hashes. UpdateHash replaces oldHash only
// if it is still current and wraps ErrHashChanged otherwise.
type Rehasher interface {
	UpdateHash(ctx context.Context, email, oldHash, newHash string) error
}

// Config configures a Verifier. Store is required.
type Config struct {
	Store Store

	// Hasher verifies and writes hashes. nil uses Argon2id with OWASP defaults.
	Hasher Hasher

	// Logger reports failed rehash writes. nil means slog.Default().
	Logger *slog.Logger

	// MaxConcurrent bounds concurrent hashing, including decoy comparisons.
	// Zero defaults to 4; negative values are invalid.
	MaxConcurrent int
}

const defaultMaxConcurrent = 4

// Verifier authenticates email/password credentials against a Store.
type Verifier struct {
	store  Store
	hasher Hasher
	log    *slog.Logger
	sem    chan struct{}
	decoy  string
}

// New validates cfg and prepares the hasher and unknown-email decoy.
func New(cfg Config) (*Verifier, error) {
	if cfg.Store == nil {
		return nil, errors.New("password: Config.Store is required")
	}
	if cfg.MaxConcurrent < 0 {
		return nil, fmt.Errorf("password: MaxConcurrent %d is negative", cfg.MaxConcurrent)
	}
	h := cfg.Hasher
	if h == nil {
		h = Argon2id{}
	}
	if err := selfCheck(h); err != nil {
		return nil, err
	}
	probe := make([]byte, 16)
	rand.Read(probe)
	decoy, err := h.Hash(hex.EncodeToString(probe))
	if err != nil {
		return nil, fmt.Errorf("password: decoy hash: %w", err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent == 0 {
		maxConcurrent = defaultMaxConcurrent
	}
	return &Verifier{
		store:  cfg.Store,
		hasher: h,
		log:    log,
		sem:    make(chan struct{}, maxConcurrent),
		decoy:  decoy,
	}, nil
}

// Authenticate verifies email and password and returns cloned claims. Invalid
// input and credentials return ErrInvalidCredentials. Store, hash, and context
// errors remain distinct. Rehash failures are best-effort and do not fail login.
func (v *Verifier) Authenticate(ctx context.Context, email, password string) (*auth.ClaimSet, error) {
	if email == "" || len(email) > MaxEmailLen || password == "" || len(password) > MaxPasswordLen {
		return nil, ErrInvalidCredentials
	}
	select {
	case v.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("password: %w", ctx.Err())
	}
	defer func() { <-v.sem }()

	cred, err := v.store.Lookup(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Match the cost of a current-profile password mismatch. Legacy
			// hashes retain their encoded cost until rehashed.
			_ = v.hasher.Compare(v.decoy, password)
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	if err := v.hasher.Compare(cred.Hash, password); err != nil {
		if errors.Is(err, ErrMismatch) {
			return nil, ErrInvalidCredentials
		}
		return nil, err
	}
	v.maybeRehash(ctx, email, cred.Hash, password)
	return cred.Claims.Clone(), nil
}

// maybeRehash skips a CAS miss because the verified hash is no longer current.
func (v *Verifier) maybeRehash(ctx context.Context, email, verifiedHash, password string) {
	rehasher, ok := v.store.(Rehasher)
	if !ok || !v.hasher.NeedsRehash(verifiedHash) {
		return
	}
	newHash, err := v.hasher.Hash(password)
	if err != nil {
		v.log.WarnContext(ctx, "password: rehash failed", "error", err)
		return
	}
	if err := rehasher.UpdateHash(ctx, email, verifiedHash, newHash); err != nil && !errors.Is(err, ErrHashChanged) {
		v.log.WarnContext(ctx, "password: rehash write failed", "error", err)
	}
}
