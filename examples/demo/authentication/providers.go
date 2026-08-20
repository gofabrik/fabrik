package authentication

import (
	"time"

	"github.com/gofabrik/fabrik/auth"
	"github.com/gofabrik/fabrik/auth/password"
	sessionauth "github.com/gofabrik/fabrik/auth/session"
	"github.com/gofabrik/fabrik/ratelimit"
	"github.com/gofabrik/fabrik/session"
)

//fabrik:provider
func NewSessionAuth(m *session.Manager) (*sessionauth.Auth, error) {
	return sessionauth.New(m, sessionauth.Options{})
}

//fabrik:provider
func NewPasswordVerifier() (*password.Verifier, error) {
	store := password.NewMemoryStore()
	hasher := password.Argon2id{}

	adminHash, err := hasher.Hash("admin")
	if err != nil {
		return nil, err
	}
	store.Put("admin@example.com", password.Credential{
		Hash:   adminHash,
		Claims: auth.ClaimSet{Subject: "admin", Roles: []string{"admin"}},
	})

	viewerHash, err := hasher.Hash("viewer")
	if err != nil {
		return nil, err
	}
	store.Put("viewer@example.com", password.Credential{
		Hash:   viewerHash,
		Claims: auth.ClaimSet{Subject: "viewer", Roles: []string{"viewer"}},
	})

	return password.New(password.Config{Store: store})
}

//fabrik:provider
func NewLoginLimiter(store *ratelimit.MemoryStore) (*ratelimit.Limiter, error) {
	return ratelimit.New(ratelimit.PerMinute(5), store, ratelimit.WithNamespace("login"))
}

//fabrik:provider name=loginip
func NewLoginIPLimiter(store *ratelimit.MemoryStore) (*ratelimit.Limiter, error) {
	return ratelimit.New(ratelimit.Limit{Rate: 30, Period: time.Minute, Burst: 20}, store, ratelimit.WithNamespace("login-ip"))
}
