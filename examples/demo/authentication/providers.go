package authentication

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/password"
	sessionauth "github.com/gofabrik/fabrik/auth/session"
	"github.com/gofabrik/fabrik/auth/store"
	storesqlite "github.com/gofabrik/fabrik/auth/store/sqlite"
	"github.com/gofabrik/fabrik/ratelimit"
	"github.com/gofabrik/fabrik/session"
)

//fabrik:provider
func NewSessionAuth(m *session.Manager) (*sessionauth.Auth, error) {
	return sessionauth.New(m, sessionauth.Options{})
}

// Accounts lazily seeds demo accounts before credential operations.
type Accounts struct {
	store *storesqlite.Store
	seed  func() error
}

//fabrik:inject db name=database
//fabrik:provider
func NewAccounts(db *sql.DB) (*Accounts, error) {
	// Migration 0006_auth.sql owns the schema.
	s, err := storesqlite.New(db, storesqlite.Options{})
	if err != nil {
		return nil, err
	}
	a := &Accounts{store: s}
	a.seed = sync.OnceValue(func() error { return seedAccounts(context.Background(), s) })
	return a, nil
}

//fabrik:provider
func NewPasswordVerifier(accounts *Accounts) (*password.Verifier, error) {
	return password.New(password.Config{Store: accounts})
}

func (a *Accounts) Lookup(ctx context.Context, email string) (password.Credential, error) {
	if err := a.seed(); err != nil {
		return password.Credential{}, err
	}
	return a.store.Lookup(ctx, email)
}

func (a *Accounts) UpdateHash(ctx context.Context, email, oldHash, newHash string) error {
	if err := a.seed(); err != nil {
		return err
	}
	return a.store.UpdateHash(ctx, email, oldHash, newHash)
}

// Register creates a viewer account under a random opaque id and
// returns its login claims. A rejected credential write triggers a
// best-effort removal of the just-created identity.
func (a *Accounts) Register(ctx context.Context, email, hash string) (*auth.ClaimSet, error) {
	if err := a.seed(); err != nil {
		return nil, err
	}
	id, err := newIdentityID()
	if err != nil {
		return nil, err
	}
	if _, err := a.store.CreateIdentity(ctx, id, store.IdentityClaims{Roles: []string{"viewer"}}); err != nil {
		return nil, err
	}
	if err := a.store.CreatePassword(ctx, id, email, hash); err != nil {
		// Cleanup ignores request cancellation to avoid leaving an empty identity.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		a.store.DeleteIdentity(cleanupCtx, id) //nolint:errcheck // best-effort cleanup of the empty identity
		return nil, err
	}
	return &auth.ClaimSet{Subject: id, Roles: []string{"viewer"}}, nil
}

func newIdentityID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// seedAccounts uses precomputed hashes to add missing demo accounts without
// replacing credentials or bypassing the verifier's concurrency limit.
func seedAccounts(ctx context.Context, accounts *storesqlite.Store) error {
	for _, seed := range []struct {
		id, email, hash string
		claims          store.IdentityClaims
	}{
		// The password is "admin".
		{"admin", "admin@example.com",
			"$argon2id$v=19$m=19456,t=2,p=1$CDIBmn2Q8NroM7bB5jO2pA$7tetND7jpcJVXaii7IUXdc1RcYsRtkqHYrEWyqMyDyw",
			store.IdentityClaims{Roles: []string{"admin"}}},
		// The password is "viewer".
		{"viewer", "viewer@example.com",
			"$argon2id$v=19$m=19456,t=2,p=1$1ApJs1iKDhgN9FseyRvKQg$+IBkX4w9Ww1/TvMmJCKcSwP65IPTvQJCInFazR3V0Eo",
			store.IdentityClaims{Roles: []string{"viewer"}}},
	} {
		if _, err := accounts.CreateIdentity(ctx, seed.id, seed.claims); err != nil && !errors.Is(err, store.ErrExists) {
			return err
		}
		if err := accounts.CreatePassword(ctx, seed.id, seed.email, seed.hash); err != nil && !errors.Is(err, store.ErrExists) {
			return err
		}
	}
	return nil
}

//fabrik:provider
func NewLoginLimiter(store *ratelimit.MemoryStore) (*ratelimit.Limiter, error) {
	return ratelimit.New(ratelimit.PerMinute(5), store, ratelimit.WithNamespace("login"))
}

//fabrik:provider name=loginip
func NewLoginIPLimiter(store *ratelimit.MemoryStore) (*ratelimit.Limiter, error) {
	return ratelimit.New(ratelimit.Limit{Rate: 30, Period: time.Minute, Burst: 20}, store, ratelimit.WithNamespace("login-ip"))
}

//fabrik:provider name=registerip
func NewRegisterIPLimiter(store *ratelimit.MemoryStore) (*ratelimit.Limiter, error) {
	return ratelimit.New(ratelimit.Limit{Rate: 10, Period: time.Minute, Burst: 5}, store, ratelimit.WithNamespace("register-ip"))
}
