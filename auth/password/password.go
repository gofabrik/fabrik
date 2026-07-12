// Package password authenticates users against a [Store]'s email +
// password-hash table and writes identity through a [Sink] on
// success. It exposes three HTTP handlers - Login, Register, Logout -
// and mounts nothing: the app names the paths and attaches
// middleware (rate limiting, CSRF) at registration, where every
// other route gets its middleware.
//
//	ph, err := password.New(userStore, sink, password.Options{})
//	mux.HandleFunc("POST /login", ratelimited(ph.Login))
//	mux.HandleFunc("POST /register", ratelimited(ph.Register))
//	mux.HandleFunc("POST /logout", ph.Logout)
//
// The provider does not know about sessions: it hands a finished
// [auth.Identity] to the Sink. The auth/session bridge is the Sink;
// a future
// oauth2 backend is another writer to it.
package password

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"

	"github.com/gofabrik/fabrik/auth"

	"golang.org/x/crypto/bcrypt"
)

// ProviderName is the value placed in [auth.Identity.Provider] on
// every identity this backend issues.
const ProviderName = "password"

// Sink receives the identity a successful login or register
// establishes, and ends it on logout. The auth/session bridge
// satisfies it. The interface is declared here - the consumer owns
// it - so password never imports session.
type Sink interface {
	Login(ctx context.Context, id auth.Identity) error
	Logout(ctx context.Context) error
}

// DefaultMaxBodyBytes caps the credential body when
// [Options.MaxBodyBytes] is zero.
const DefaultMaxBodyBytes int64 = 64 << 10

// Options configures a [Provider]. All fields are zero-safe.
type Options struct {
	// OnSuccess runs after login, auto-login from register, and
	// logout. Default: 204 No Content, empty body. For logout the
	// id is the zero auth.Identity (Subject == ""). It returns an
	// error: do fallible post-auth work (a flash write, a redirect
	// lookup) and return the error before writing the response.
	// The session write is already staged by the time OnSuccess
	// runs, so a returned error means "the user is authenticated
	// but the follow-up failed" - fail the request loudly, do not
	// pretend the login did not happen. The provider routes the
	// error through OnFailure as an OpHook failure.
	OnSuccess func(w http.ResponseWriter, r *http.Request, id auth.Identity) error

	// OnFailure runs when a handler fails. The error is a [*Error]
	// wrapping the stage, or a sentinel ([ErrInvalidCredentials],
	// [ErrPasswordTooShort], [ErrEmailTaken]). Default:
	// [DefaultOnFailure].
	OnFailure func(w http.ResponseWriter, r *http.Request, err error)

	// Parser extracts credentials from the body. Default:
	// [DefaultParser] (JSON/form auto-detect).
	Parser Parser

	// OnRegistered runs after a successful Store.Create, receiving
	// the same identity a login emits (not the store's User). The
	// default auto-logs-in through the sink; a custom hook replaces
	// that entirely (welcome email, manual verification, custom
	// auto-login) - capture the sink in the closure if it wants to
	// log in. Like OnSuccess it returns an error: do fallible
	// post-create work and return the error before writing the
	// response; the provider routes it through OnFailure as an
	// OpHook failure.
	OnRegistered func(w http.ResponseWriter, r *http.Request, id auth.Identity) error

	// MinPasswordLength rejects shorter passwords at register time.
	// Default 8; never applied to login.
	MinPasswordLength int

	// MaxBodyBytes caps the credential body. Default 64 KiB.
	MaxBodyBytes int64

	// Hasher overrides the default bcrypt(cost=12) hasher.
	Hasher Hasher
}

// Provider is the password backend. Construct with [New]; register
// its handler methods on your router.
type Provider struct {
	store  Store
	sink   Sink
	hasher Hasher
	parser Parser

	// dummyHash is minted once at New via the configured hasher so
	// the unknown-email path verifies against a same-cost hash - no
	// timing gap a caller could use to enumerate emails. This assumes
	// stored hashes share that cost; a store still holding hashes at
	// an older bcrypt cost verifies in a different time than the dummy
	// and reopens a narrow timing signal. Rehash-on-login to the
	// current cost (or a one-off migration) closes it.
	dummyHash []byte

	onSuccess    func(w http.ResponseWriter, r *http.Request, id auth.Identity) error
	onFailure    func(w http.ResponseWriter, r *http.Request, err error)
	onRegistered func(w http.ResponseWriter, r *http.Request, id auth.Identity) error

	minPasswordLength int
	maxBodyBytes      int64
}

// New returns a configured [Provider]. It returns an error rather
// than panicking: nil (or typed-nil) store or sink are boot errors,
// and the boot-time dummy-hash mint can fail with a custom hasher.
func New(store Store, sink Sink, opts Options) (*Provider, error) {
	if store == nil || isNilValue(store) {
		return nil, errors.New("password.New: nil Store")
	}
	if sink == nil || isNilValue(sink) {
		return nil, errors.New("password.New: nil Sink")
	}

	p := &Provider{
		store:             store,
		sink:              sink,
		hasher:            opts.Hasher,
		parser:            opts.Parser,
		onSuccess:         opts.OnSuccess,
		onFailure:         opts.OnFailure,
		onRegistered:      opts.OnRegistered,
		minPasswordLength: opts.MinPasswordLength,
		maxBodyBytes:      opts.MaxBodyBytes,
	}
	switch {
	case opts.Hasher == nil:
		p.hasher = BcryptHasher{Cost: DefaultBcryptCost}
	case isNilValue(opts.Hasher):
		return nil, errors.New("password.New: typed-nil Hasher")
	}
	if p.parser == nil {
		p.parser = DefaultParser
	}
	if p.onSuccess == nil {
		p.onSuccess = defaultOnSuccess
	}
	if p.onFailure == nil {
		p.onFailure = DefaultOnFailure
	}
	if p.onRegistered == nil {
		p.onRegistered = p.autoLogin
	}
	if p.minPasswordLength <= 0 {
		p.minPasswordLength = 8
	}
	if p.maxBodyBytes <= 0 {
		p.maxBodyBytes = DefaultMaxBodyBytes
	}

	dummy, err := p.hasher.Hash("")
	if err != nil {
		return nil, fmt.Errorf("password.New: precompute dummy hash: %w", err)
	}
	p.dummyHash = dummy

	return p, nil
}

// Login verifies credentials and, on success, logs the user in
// through the sink.
func (p *Provider) Login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, p.maxBodyBytes)
	creds, err := p.parser(r)
	if err != nil {
		p.onFailure(w, r, opError(OpParse, err))
		return
	}
	if creds.Email == "" || creds.Password == "" {
		p.onFailure(w, r, ErrInvalidCredentials)
		return
	}

	u, err := p.store.LookupByEmail(r.Context(), creds.Email)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		p.onFailure(w, r, opError(OpLookup, err))
		return
	}
	if errors.Is(err, ErrUserNotFound) {
		// Timing-safe: verify against the same-cost dummy so the
		// response time matches a real failed compare.
		_ = p.hasher.Verify(creds.Password, p.dummyHash)
		p.onFailure(w, r, ErrInvalidCredentials)
		return
	}
	if !p.hasher.Verify(creds.Password, u.PassHash) {
		p.onFailure(w, r, ErrInvalidCredentials)
		return
	}

	id, err := p.identityFor(u)
	if err != nil {
		p.onFailure(w, r, opError(OpLookup, err))
		return
	}
	if err := p.sink.Login(r.Context(), id); err != nil {
		p.onFailure(w, r, opError(OpSession, err))
		return
	}
	if err := p.onSuccess(w, r, id); err != nil {
		p.onFailure(w, r, opError(OpHook, err))
	}
}

// Register creates a user and runs OnRegistered (default:
// auto-login). Always callable - an app that does not want
// registration simply does not route this handler.
func (p *Provider) Register(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, p.maxBodyBytes)
	creds, err := p.parser(r)
	if err != nil {
		p.onFailure(w, r, opError(OpParse, err))
		return
	}
	if creds.Email == "" || creds.Password == "" {
		p.onFailure(w, r, ErrInvalidCredentials)
		return
	}
	if len(creds.Password) < p.minPasswordLength {
		p.onFailure(w, r, &PasswordTooShortError{Min: p.minPasswordLength})
		return
	}

	hash, err := p.hasher.Hash(creds.Password)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			// bcrypt caps at 72 bytes; that is a password-policy
			// rejection the user can fix, not a server error.
			p.onFailure(w, r, &PasswordTooLongError{Max: 72})
			return
		}
		p.onFailure(w, r, opError(OpHash, err))
		return
	}

	u, err := p.store.Create(r.Context(), creds.Email, hash)
	if err != nil {
		if errors.Is(err, ErrEmailTaken) {
			p.onFailure(w, r, err) // expected, non-enumerating
			return
		}
		p.onFailure(w, r, opError(OpCreate, err))
		return
	}

	id, err := p.identityFor(u)
	if err != nil {
		p.onFailure(w, r, opError(OpCreate, err))
		return
	}
	if err := p.onRegistered(w, r, id); err != nil {
		p.onFailure(w, r, hookError(err))
	}
}

// Logout ends the session through the sink. Idempotent on a visitor
// with no live session.
func (p *Provider) Logout(w http.ResponseWriter, r *http.Request) {
	if err := p.sink.Logout(r.Context()); err != nil {
		p.onFailure(w, r, opError(OpLogout, err))
		return
	}
	if err := p.onSuccess(w, r, auth.Identity{}); err != nil {
		p.onFailure(w, r, opError(OpHook, err))
	}
}

// autoLogin is the default OnRegistered: log the new user in and run
// OnSuccess. It classifies its own failures - a failed sink Login is
// a session failure, a failed OnSuccess is a follow-up - so Register
// passes them through unchanged.
func (p *Provider) autoLogin(w http.ResponseWriter, r *http.Request, id auth.Identity) error {
	if err := p.sink.Login(r.Context(), id); err != nil {
		return opError(OpSession, err)
	}
	if err := p.onSuccess(w, r, id); err != nil {
		return opError(OpHook, err)
	}
	return nil
}

// hookError classifies a hook's returned error: an already-typed
// *Error (the default autoLogin pre-classifies its sink failure as
// OpSession) passes through; a bare error from a custom hook is an
// OpHook follow-up failure.
func hookError(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	return opError(OpHook, err)
}

// identityFor builds the pinned identity a login or register emits.
// An empty User.ID is a store-contract violation - Subject must be
// non-empty.
func (p *Provider) identityFor(u User) (auth.Identity, error) {
	if u.ID == "" {
		return auth.Identity{}, errors.New("password: store returned an empty User.ID")
	}
	return auth.Identity{
		Subject:  u.ID,
		Email:    u.Email,
		Provider: ProviderName,
	}, nil
}

// defaultOnSuccess writes 204 No Content.
func defaultOnSuccess(w http.ResponseWriter, _ *http.Request, _ auth.Identity) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// DefaultOnFailure routes the wire response: session/logout stages
// to 500, ErrPasswordTooShort to 400, everything else to the
// non-enumerating 401. Operational causes log via slog.Warn first;
// expected credential/privacy outcomes do not.
func DefaultOnFailure(w http.ResponseWriter, r *http.Request, err error) {
	logIfOperational(r, err)

	if IsOperational(err) {
		// A store outage or hashing failure is a server error, not a
		// credential rejection. It does not enable enumeration - it
		// happens regardless of whether the account exists.
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	switch {
	case errors.Is(err, ErrPasswordTooShort):
		http.Error(w, "password too short", http.StatusBadRequest)
	case errors.Is(err, ErrPasswordTooLong):
		http.Error(w, "password too long", http.StatusBadRequest)
	default:
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
	}
}

// logIfOperational logs the causes an operator needs - store
// outages, hashing failures, session errors - while leaving expected
// credential and privacy outcomes (bad password, taken email, too
// short) and client-driven parse noise unlogged.
func logIfOperational(r *http.Request, err error) {
	if !IsOperational(err) {
		return
	}
	var pe *Error
	_ = errors.As(err, &pe)
	slog.WarnContext(r.Context(), "password: operational failure", "op", string(pe.Op), "err", pe.Err.Error())
}

// isNilValue detects a typed-nil interface value.
func isNilValue(v any) bool {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Func, reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	}
	return false
}
