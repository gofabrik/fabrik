package authentication

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

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

//fabrik:inject db name=database
//fabrik:provider
func NewPasswordVerifier(db *sql.DB) (*password.Verifier, error) {
	// Migration 0006_auth.sql owns the schema.
	accounts, err := storesqlite.New(db, storesqlite.Options{})
	if err != nil {
		return nil, err
	}
	return password.New(password.Config{Store: &seededAccounts{
		Store: accounts,
		seed:  sync.OnceValue(func() error { return seedAccounts(context.Background(), accounts) }),
	}})
}

// seededAccounts keeps commands that do not authenticate from writing account tables.
type seededAccounts struct {
	*storesqlite.Store
	seed func() error
}

func (s *seededAccounts) Lookup(ctx context.Context, email string) (password.Credential, error) {
	if err := s.seed(); err != nil {
		return password.Credential{}, err
	}
	return s.Store.Lookup(ctx, email)
}

// seedAccounts creates missing demo accounts without replacing existing credentials.
func seedAccounts(ctx context.Context, accounts *storesqlite.Store) error {
	hasher := password.Argon2id{}
	for _, seed := range []struct {
		id, email, password string
		claims              store.IdentityClaims
	}{
		{"admin", "admin@example.com", "admin", store.IdentityClaims{Roles: []string{"admin"}}},
		{"viewer", "viewer@example.com", "viewer", store.IdentityClaims{Roles: []string{"viewer"}}},
	} {
		if _, err := accounts.CreateIdentity(ctx, seed.id, seed.claims); err != nil && !errors.Is(err, store.ErrExists) {
			return err
		}
		hash, err := hasher.Hash(seed.password)
		if err != nil {
			return err
		}
		if err := accounts.CreatePassword(ctx, seed.id, seed.email, hash); err != nil && !errors.Is(err, store.ErrExists) {
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
