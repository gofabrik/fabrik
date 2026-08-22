// Package storetest provides a conformance suite for [store.Store] implementations.
package storetest

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/auth/password"
	"github.com/gofabrik/fabrik/auth/store"
)

// Factory returns a fresh empty store; a nil clock requests the implementation default.
type Factory func(t *testing.T, now func() time.Time) store.Store

// Run executes the conformance suite with a fresh store per subtest.
func Run(t *testing.T, newStore Factory) {
	t.Helper()
	t.Run("CreateIdentityRoundTrip", func(t *testing.T) { testCreateIdentityRoundTrip(t, newStore) })
	t.Run("CreateIdentityDuplicate", func(t *testing.T) { testCreateIdentityDuplicate(t, newStore) })
	t.Run("IdentityIDValidation", func(t *testing.T) { testIdentityIDValidation(t, newStore) })
	t.Run("ByteExactIdentityIDs", func(t *testing.T) { testByteExactIdentityIDs(t, newStore) })
	t.Run("GetIdentityMissing", func(t *testing.T) { testGetIdentityMissing(t, newStore) })
	t.Run("SetIdentityStatus", func(t *testing.T) { testSetIdentityStatus(t, newStore) })
	t.Run("SetIdentityClaims", func(t *testing.T) { testSetIdentityClaims(t, newStore) })
	t.Run("DeleteIdentityIdempotent", func(t *testing.T) { testDeleteIdentityIdempotent(t, newStore) })
	t.Run("DeleteIdentityCascades", func(t *testing.T) { testDeleteIdentityCascades(t, newStore) })
	t.Run("DeleteThenRecreateIdentity", func(t *testing.T) { testDeleteThenRecreateIdentity(t, newStore) })
	t.Run("CreatePassword", func(t *testing.T) { testCreatePassword(t, newStore) })
	t.Run("CreatePasswordConflicts", func(t *testing.T) { testCreatePasswordConflicts(t, newStore) })
	t.Run("CreatePasswordConcurrent", func(t *testing.T) { testCreatePasswordConcurrent(t, newStore) })
	t.Run("SetPassword", func(t *testing.T) { testSetPassword(t, newStore) })
	t.Run("PasswordValidation", func(t *testing.T) { testPasswordValidation(t, newStore) })
	t.Run("EmailNormalization", func(t *testing.T) { testEmailNormalization(t, newStore) })
	t.Run("EmailBoundaryLength", func(t *testing.T) { testEmailBoundaryLength(t, newStore) })
	t.Run("LookupSeam", func(t *testing.T) { testLookupSeam(t, newStore) })
	t.Run("ClaimsNarrowing", func(t *testing.T) { testClaimsNarrowing(t, newStore) })
	t.Run("LargeValues", func(t *testing.T) { testLargeValues(t, newStore) })
	t.Run("CopyIsolation", func(t *testing.T) { testCopyIsolation(t, newStore) })
	t.Run("Timestamps", func(t *testing.T) { testTimestamps(t, newStore) })
	t.Run("UpdateHashCAS", func(t *testing.T) { testUpdateHashCAS(t, newStore) })
	t.Run("UpdateHashConcurrent", func(t *testing.T) { testUpdateHashConcurrent(t, newStore) })
	t.Run("VerifierAuthenticate", func(t *testing.T) { testVerifierAuthenticate(t, newStore) })
	t.Run("VerifierRehash", func(t *testing.T) { testVerifierRehash(t, newStore) })
	t.Run("VerifierRehashLosesRace", func(t *testing.T) { testVerifierRehashLosesRace(t, newStore) })
}

// lightHasher reduces the cost of verifier integration tests.
func lightHasher() password.Hasher {
	return password.Argon2id{Time: 1, MemoryKiB: 8192}
}

func mustCreate(t *testing.T, s store.Store, id string, claims store.IdentityClaims) store.Identity {
	t.Helper()
	ident, err := s.CreateIdentity(context.Background(), id, claims)
	if err != nil {
		t.Fatalf("CreateIdentity(%q): %v", id, err)
	}
	return ident
}

func mustCreatePassword(t *testing.T, s store.Store, id, email, hash string) {
	t.Helper()
	if err := s.CreatePassword(context.Background(), id, email, hash); err != nil {
		t.Fatalf("CreatePassword(%q, %q): %v", id, email, err)
	}
}

func testCreateIdentityRoundTrip(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	claims := store.IdentityClaims{
		Roles:        []string{"admin", "editor"},
		Groups:       []string{"staff"},
		Entitlements: []string{"beta"},
	}
	created := mustCreate(t, s, "alice", claims)
	if created.ID != "alice" {
		t.Fatalf("created ID = %q, want alice", created.ID)
	}
	if created.Status != store.StatusActive {
		t.Fatalf("created Status = %q, want active", created.Status)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("created timestamps zero: %v %v", created.CreatedAt, created.UpdatedAt)
	}
	got, err := s.GetIdentity(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	assertClaims(t, got.Claims, claims)
	if got.Status != store.StatusActive {
		t.Fatalf("got Status = %q, want active", got.Status)
	}
	if got.CreatedAt.UnixNano() != created.CreatedAt.UnixNano() {
		t.Fatalf("CreatedAt drifted: %v vs %v", got.CreatedAt, created.CreatedAt)
	}
}

func testCreateIdentityDuplicate(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{Roles: []string{"admin"}})
	_, err := s.CreateIdentity(ctx, "alice", store.IdentityClaims{})
	if !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate CreateIdentity = %v, want ErrExists", err)
	}
	got, err := s.GetIdentity(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	assertClaims(t, got.Claims, store.IdentityClaims{Roles: []string{"admin"}})
}

func testIdentityIDValidation(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	longest := strings.Repeat("a", store.MaxIdentityIDLen)
	if _, err := s.CreateIdentity(ctx, longest, store.IdentityClaims{}); err != nil {
		t.Fatalf("CreateIdentity(boundary id): %v", err)
	}
	for name, call := range map[string]func(id string) error{
		"CreateIdentity": func(id string) error {
			_, err := s.CreateIdentity(ctx, id, store.IdentityClaims{})
			return err
		},
		"GetIdentity": func(id string) error {
			_, err := s.GetIdentity(ctx, id)
			return err
		},
		"SetIdentityStatus": func(id string) error {
			return s.SetIdentityStatus(ctx, id, store.StatusDisabled)
		},
		"SetIdentityClaims": func(id string) error {
			return s.SetIdentityClaims(ctx, id, store.IdentityClaims{})
		},
		"DeleteIdentity": func(id string) error {
			return s.DeleteIdentity(ctx, id)
		},
		"CreatePassword": func(id string) error {
			return s.CreatePassword(ctx, id, "v@example.com", "h")
		},
		"SetPassword": func(id string) error {
			return s.SetPassword(ctx, id, "v@example.com", "h")
		},
	} {
		if err := call(""); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s(empty id) = %v, want ErrInvalid", name, err)
		}
		if err := call(longest + "a"); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s(oversized id) = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.Lookup(ctx, "v@example.com"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("rejected invalid-id password write materialized a credential: %v", err)
	}
}

func testByteExactIdentityIDs(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "User1", store.IdentityClaims{Roles: []string{"upper"}})
	mustCreate(t, s, "user1", store.IdentityClaims{Roles: []string{"lower"}})
	upper, err := s.GetIdentity(ctx, "User1")
	if err != nil {
		t.Fatalf("GetIdentity(User1): %v", err)
	}
	lower, err := s.GetIdentity(ctx, "user1")
	if err != nil {
		t.Fatalf("GetIdentity(user1): %v", err)
	}
	if len(upper.Claims.Roles) != 1 || upper.Claims.Roles[0] != "upper" {
		t.Fatalf("User1 claims = %v", upper.Claims.Roles)
	}
	if len(lower.Claims.Roles) != 1 || lower.Claims.Roles[0] != "lower" {
		t.Fatalf("user1 claims = %v", lower.Claims.Roles)
	}
	mustCreate(t, s, "user1 ", store.IdentityClaims{Roles: []string{"padded"}})
	padded, err := s.GetIdentity(ctx, "user1 ")
	if err != nil {
		t.Fatalf("GetIdentity(padded): %v", err)
	}
	if len(padded.Claims.Roles) != 1 || padded.Claims.Roles[0] != "padded" {
		t.Fatalf("padded id claims = %v", padded.Claims.Roles)
	}
	plain, err := s.GetIdentity(ctx, "user1")
	if err != nil || len(plain.Claims.Roles) != 1 || plain.Claims.Roles[0] != "lower" {
		t.Fatalf("padded id collided with user1: %v, %v", plain.Claims.Roles, err)
	}

	for i, id := range []string{"u\x00id", "id\xff\xfe"} {
		tag := "binary-" + strconv.Itoa(i)
		mustCreate(t, s, id, store.IdentityClaims{Roles: []string{tag}})
		got, err := s.GetIdentity(ctx, id)
		if err != nil || len(got.Claims.Roles) != 1 || got.Claims.Roles[0] != tag {
			t.Fatalf("GetIdentity(%q) = %v, %v", id, got.Claims.Roles, err)
		}
	}

	// Identity ID limits count bytes, not runes.
	multibyte := strings.Repeat("é", (store.MaxIdentityIDLen-1)/2) + "a"
	if len(multibyte) != store.MaxIdentityIDLen {
		t.Fatalf("multibyte fixture is %d bytes, want %d", len(multibyte), store.MaxIdentityIDLen)
	}
	mustCreate(t, s, multibyte, store.IdentityClaims{})
	over := strings.Repeat("é", store.MaxIdentityIDLen/2+1)
	if _, err := s.CreateIdentity(ctx, over, store.IdentityClaims{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("CreateIdentity(%d-byte multibyte id) = %v, want ErrInvalid", len(over), err)
	}
}

func testGetIdentityMissing(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	if _, err := s.GetIdentity(context.Background(), "ghost"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetIdentity(missing) = %v, want ErrNotFound", err)
	}
}

func testSetIdentityStatus(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{})
	mustCreatePassword(t, s, "alice", "alice@example.com", testHash(t, "secret"))

	if err := s.SetIdentityStatus(ctx, "alice", store.StatusDisabled); err != nil {
		t.Fatalf("SetIdentityStatus(disabled): %v", err)
	}
	got, err := s.GetIdentity(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIdentity(disabled) = %v, want the identity", err)
	}
	if got.Status != store.StatusDisabled {
		t.Fatalf("Status = %q, want disabled", got.Status)
	}
	if _, err := s.Lookup(ctx, "alice@example.com"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("Lookup(disabled) = %v, want password.ErrNotFound", err)
	}

	if err := s.SetIdentityStatus(ctx, "alice", store.StatusActive); err != nil {
		t.Fatalf("SetIdentityStatus(active): %v", err)
	}
	if _, err := s.Lookup(ctx, "alice@example.com"); err != nil {
		t.Fatalf("Lookup(re-enabled): %v", err)
	}

	if err := s.SetIdentityStatus(ctx, "ghost", store.StatusDisabled); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetIdentityStatus(missing) = %v, want ErrNotFound", err)
	}
	before, err := s.GetIdentity(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	if err := s.SetIdentityStatus(ctx, "alice", store.Status("frozen")); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("SetIdentityStatus(frozen) = %v, want ErrInvalid", err)
	}
	got, err = s.GetIdentity(ctx, "alice")
	if err != nil || got.Status != store.StatusActive {
		t.Fatalf("rejected status write mutated the identity: %q, %v", got.Status, err)
	}
	if got.UpdatedAt.UnixNano() != before.UpdatedAt.UnixNano() {
		t.Fatalf("rejected status write moved UpdatedAt: %v", got.UpdatedAt)
	}
}

func testSetIdentityClaims(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{Roles: []string{"admin"}})
	next := store.IdentityClaims{Groups: []string{"staff"}}
	if err := s.SetIdentityClaims(ctx, "alice", next); err != nil {
		t.Fatalf("SetIdentityClaims: %v", err)
	}
	got, err := s.GetIdentity(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	assertClaims(t, got.Claims, next)
	if err := s.SetIdentityClaims(ctx, "ghost", next); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetIdentityClaims(missing) = %v, want ErrNotFound", err)
	}
}

func testDeleteIdentityIdempotent(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{})
	if err := s.DeleteIdentity(ctx, "alice"); err != nil {
		t.Fatalf("DeleteIdentity: %v", err)
	}
	if _, err := s.GetIdentity(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetIdentity(deleted) = %v, want ErrNotFound", err)
	}
	if err := s.DeleteIdentity(ctx, "alice"); err != nil {
		t.Fatalf("DeleteIdentity(again): %v", err)
	}
	if err := s.DeleteIdentity(ctx, "ghost"); err != nil {
		t.Fatalf("DeleteIdentity(missing): %v", err)
	}
}

func testDeleteIdentityCascades(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{})
	mustCreatePassword(t, s, "alice", "alice@example.com", testHash(t, "secret"))
	if err := s.DeleteIdentity(ctx, "alice"); err != nil {
		t.Fatalf("DeleteIdentity: %v", err)
	}
	if _, err := s.Lookup(ctx, "alice@example.com"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("Lookup(cascaded) = %v, want password.ErrNotFound", err)
	}
	mustCreate(t, s, "bob", store.IdentityClaims{})
	mustCreatePassword(t, s, "bob", "alice@example.com", testHash(t, "other"))
}

func testDeleteThenRecreateIdentity(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	const (
		id           = "alice"
		email        = "alice@example.com"
		passwordText = "old-secret"
	)
	mustCreate(t, s, id, store.IdentityClaims{})
	mustCreatePassword(t, s, id, email, testHash(t, passwordText))
	if err := s.DeleteIdentity(ctx, id); err != nil {
		t.Fatalf("DeleteIdentity: %v", err)
	}
	mustCreate(t, s, id, store.IdentityClaims{})
	if _, err := s.Lookup(ctx, email); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("Lookup after identity ID reuse = %v, want password.ErrNotFound", err)
	}
	v, err := password.New(password.Config{Store: s, Hasher: lightHasher()})
	if err != nil {
		t.Fatalf("password.New: %v", err)
	}
	if _, err := v.Authenticate(ctx, email, passwordText); !errors.Is(err, password.ErrInvalidCredentials) {
		t.Fatalf("Authenticate with deleted password = %v, want ErrInvalidCredentials", err)
	}
}

func testCreatePassword(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{Roles: []string{"admin"}})
	hash := testHash(t, "secret")
	mustCreatePassword(t, s, "alice", "alice@example.com", hash)
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if cred.Hash != hash {
		t.Fatalf("Lookup hash = %q, want the stored hash", cred.Hash)
	}
	if cred.Claims.Subject != "alice" {
		t.Fatalf("Lookup Subject = %q, want alice", cred.Claims.Subject)
	}
}

func testCreatePasswordConflicts(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{})
	mustCreate(t, s, "bob", store.IdentityClaims{})
	aliceHash := testHash(t, "alice-secret")
	bobHash := testHash(t, "bob-secret")
	mustCreatePassword(t, s, "alice", "alice@example.com", aliceHash)
	mustCreatePassword(t, s, "bob", "bob@example.com", bobHash)

	if err := s.CreatePassword(ctx, "ghost", "ghost@example.com", "h"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("CreatePassword(unknown identity) = %v, want ErrNotFound", err)
	}
	if err := s.CreatePassword(ctx, "alice", "alice@example.com", testHash(t, "different")); !errors.Is(err, store.ErrExists) {
		t.Fatalf("CreatePassword(existing credential) = %v, want ErrExists", err)
	}
	if err := s.CreatePassword(ctx, "alice", "alice-second@example.com", testHash(t, "different")); !errors.Is(err, store.ErrExists) {
		t.Fatalf("CreatePassword(existing credential, fresh email) = %v, want ErrExists", err)
	}
	if _, err := s.Lookup(ctx, "alice-second@example.com"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("rejected fresh email resolves: %v", err)
	}
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if cred.Hash != aliceHash {
		t.Fatalf("rejected CreatePassword changed the hash")
	}
	if err := s.CreatePassword(ctx, "carol", "bob@example.com", "h"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("CreatePassword(unknown identity, taken email) = %v, want ErrNotFound", err)
	}
	mustCreate(t, s, "carol", store.IdentityClaims{})
	if err := s.CreatePassword(ctx, "carol", "bob@example.com", "h"); !errors.Is(err, store.ErrEmailTaken) {
		t.Fatalf("CreatePassword(taken email) = %v, want ErrEmailTaken", err)
	}
	if err := s.CreatePassword(ctx, "alice", "bob@example.com", "h"); !errors.Is(err, store.ErrEmailTaken) {
		t.Fatalf("CreatePassword(own credential, taken email) = %v, want ErrEmailTaken", err)
	}
	cred, err = s.Lookup(ctx, "bob@example.com")
	if err != nil {
		t.Fatalf("Lookup(bob): %v", err)
	}
	if cred.Hash != bobHash || cred.Claims.Subject != "bob" {
		t.Fatalf("conflicting CreatePassword touched bob's credential")
	}
}

func testCreatePasswordConcurrent(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{})
	const n = 8
	email := func(i int) string { return "alice-" + strconv.Itoa(i) + "@example.com" }
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.CreatePassword(ctx, "alice", email(i), "hash-"+strconv.Itoa(i))
		}()
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner != -1 {
				t.Fatalf("concurrent CreatePassword admitted %d and %d", winner, i)
			}
			winner = i
		case errors.Is(err, store.ErrExists):
		default:
			t.Fatalf("concurrent CreatePassword[%d] = %v", i, err)
		}
	}
	if winner == -1 {
		t.Fatalf("concurrent CreatePassword admitted no winner")
	}
	cred, err := s.Lookup(ctx, email(winner))
	if err != nil {
		t.Fatalf("Lookup(winner): %v", err)
	}
	if cred.Hash != "hash-"+strconv.Itoa(winner) {
		t.Fatalf("persisted hash %q is not the winner's", cred.Hash)
	}
	for i := range n {
		if i == winner {
			continue
		}
		if _, err := s.Lookup(ctx, email(i)); !errors.Is(err, password.ErrNotFound) {
			t.Fatalf("loser email %d resolves: %v", i, err)
		}
	}
}

func testSetPassword(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{})
	mustCreate(t, s, "bob", store.IdentityClaims{})
	mustCreatePassword(t, s, "bob", "bob@example.com", testHash(t, "bob-secret"))

	first := testHash(t, "first")
	if err := s.SetPassword(ctx, "alice", "alice@example.com", first); err != nil {
		t.Fatalf("SetPassword(no prior credential): %v", err)
	}
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil || cred.Hash != first {
		t.Fatalf("Lookup after SetPassword = %q, %v", cred.Hash, err)
	}

	second := testHash(t, "second")
	if err := s.SetPassword(ctx, "alice", "alice@example.com", second); err != nil {
		t.Fatalf("SetPassword(same email, new hash): %v", err)
	}
	cred, err = s.Lookup(ctx, "alice@example.com")
	if err != nil || cred.Hash != second {
		t.Fatalf("Lookup after replace = %q, %v", cred.Hash, err)
	}

	third := testHash(t, "third")
	if err := s.SetPassword(ctx, "alice", "renamed@example.com", third); err != nil {
		t.Fatalf("SetPassword(new email): %v", err)
	}
	if _, err := s.Lookup(ctx, "alice@example.com"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("Lookup(old email) = %v, want password.ErrNotFound", err)
	}
	cred, err = s.Lookup(ctx, "renamed@example.com")
	if err != nil || cred.Hash != third {
		t.Fatalf("Lookup(new email) = %q, %v", cred.Hash, err)
	}

	bobHash := testHash(t, "bob-replacement")
	if err := s.SetPassword(ctx, "bob", "bob@example.com", bobHash); err != nil {
		t.Fatalf("SetPassword(bob): %v", err)
	}
	if err := s.SetPassword(ctx, "alice", "bob@example.com", "h"); !errors.Is(err, store.ErrEmailTaken) {
		t.Fatalf("SetPassword(taken email) = %v, want ErrEmailTaken", err)
	}
	if err := s.SetPassword(ctx, "ghost", "bob@example.com", "h"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetPassword(unknown identity, taken email) = %v, want ErrNotFound", err)
	}
	cred, err = s.Lookup(ctx, "bob@example.com")
	if err != nil || cred.Claims.Subject != "bob" || cred.Hash != bobHash {
		t.Fatalf("conflicting SetPassword touched bob's credential: %q, %v", cred.Hash, err)
	}
	cred, err = s.Lookup(ctx, "renamed@example.com")
	if err != nil || cred.Hash != third {
		t.Fatalf("rejected SetPassword touched alice's credential: %q, %v", cred.Hash, err)
	}
}

func testPasswordValidation(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{})
	hash := testHash(t, "secret")
	mustCreatePassword(t, s, "alice", "alice@example.com", hash)
	for name, call := range map[string]func(email, hash string) error{
		"CreatePassword": func(email, hash string) error { return s.CreatePassword(ctx, "alice", email, hash) },
		"SetPassword":    func(email, hash string) error { return s.SetPassword(ctx, "alice", email, hash) },
	} {
		if err := call("", "h"); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s(empty email) = %v, want ErrInvalid", name, err)
		}
		if err := call("   ", "h"); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s(blank email) = %v, want ErrInvalid", name, err)
		}
	}
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil || cred.Hash != hash {
		t.Fatalf("rejected password write mutated the credential: %q, %v", cred.Hash, err)
	}
}

func testEmailNormalization(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{})
	mustCreate(t, s, "bob", store.IdentityClaims{})
	hash := testHash(t, "secret")
	mustCreatePassword(t, s, "alice", "  Alice@Example.COM ", hash)
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil || cred.Hash != hash {
		t.Fatalf("Lookup(normalized) = %v", err)
	}
	if _, err := s.Lookup(ctx, " ALICE@example.com"); err != nil {
		t.Fatalf("Lookup(denormalized query): %v", err)
	}
	if err := s.CreatePassword(ctx, "bob", "ALICE@EXAMPLE.COM", "h"); !errors.Is(err, store.ErrEmailTaken) {
		t.Fatalf("CreatePassword(case-variant taken email) = %v, want ErrEmailTaken", err)
	}
	replaced := testHash(t, "replaced")
	if err := s.SetPassword(ctx, "alice", "  ALICE@Example.com ", replaced); err != nil {
		t.Fatalf("SetPassword(denormalized email): %v", err)
	}
	cred, err = s.Lookup(ctx, "alice@example.com")
	if err != nil || cred.Hash != replaced {
		t.Fatalf("Lookup after denormalized SetPassword = %q, %v", cred.Hash, err)
	}
	next := testHash(t, "next")
	if err := s.UpdateHash(ctx, " Alice@EXAMPLE.com", replaced, next); err != nil {
		t.Fatalf("UpdateHash(denormalized email): %v", err)
	}
	cred, err = s.Lookup(ctx, "alice@example.com")
	if err != nil || cred.Hash != next {
		t.Fatalf("Lookup after denormalized UpdateHash = %q, %v", cred.Hash, err)
	}
}

func testEmailBoundaryLength(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{})
	domain := "@example.com"
	longest := strings.Repeat("a", password.MaxEmailLen-len(domain)) + domain
	if err := s.CreatePassword(ctx, "alice", " "+longest, "h"); err != nil {
		t.Fatalf("CreatePassword(boundary email, raw over limit): %v", err)
	}
	mustCreate(t, s, "bob", store.IdentityClaims{})
	if err := s.CreatePassword(ctx, "bob", "a"+longest, "h"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("CreatePassword(oversized email) = %v, want ErrInvalid", err)
	}
	if err := s.SetPassword(ctx, "bob", "a"+longest, "h"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("SetPassword(oversized email) = %v, want ErrInvalid", err)
	}
	// Rejected writes must not create a credential.
	mustCreatePassword(t, s, "bob", "bob@example.com", "h")
}

func testLookupSeam(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	if _, err := s.Lookup(ctx, "ghost@example.com"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("Lookup(unknown) = %v, want password.ErrNotFound", err)
	}
	if _, err := s.Lookup(ctx, "   "); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("Lookup(blank) = %v, want password.ErrNotFound", err)
	}
	if _, err := s.Lookup(ctx, strings.Repeat("a", password.MaxEmailLen+1)); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("Lookup(oversized) = %v, want password.ErrNotFound", err)
	}
	if err := s.UpdateHash(ctx, "ghost@example.com", "old", "new"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("UpdateHash(unknown) = %v, want password.ErrNotFound", err)
	}
	if err := s.UpdateHash(ctx, strings.Repeat("a", password.MaxEmailLen+1), "old", "new"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("UpdateHash(oversized) = %v, want password.ErrNotFound", err)
	}
	if err := s.UpdateHash(ctx, "   ", "old", "new"); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("UpdateHash(blank) = %v, want password.ErrNotFound", err)
	}
	mustCreate(t, s, "alice", store.IdentityClaims{})
	hash := testHash(t, "secret")
	mustCreatePassword(t, s, "alice", "alice@example.com", hash)
	if err := s.SetIdentityStatus(ctx, "alice", store.StatusDisabled); err != nil {
		t.Fatalf("SetIdentityStatus: %v", err)
	}
	if err := s.UpdateHash(ctx, "alice@example.com", hash, testHash(t, "next")); !errors.Is(err, password.ErrNotFound) {
		t.Fatalf("UpdateHash(disabled) = %v, want password.ErrNotFound", err)
	}
	if err := s.SetIdentityStatus(ctx, "alice", store.StatusActive); err != nil {
		t.Fatalf("SetIdentityStatus: %v", err)
	}
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil || cred.Hash != hash {
		t.Fatalf("rejected disabled UpdateHash mutated the hash: %q, %v", cred.Hash, err)
	}
}

func testClaimsNarrowing(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	claims := store.IdentityClaims{
		Roles:        []string{"admin"},
		Groups:       []string{"staff"},
		Entitlements: []string{"beta"},
	}
	mustCreate(t, s, "alice", claims)
	mustCreatePassword(t, s, "alice", "alice@example.com", testHash(t, "secret"))
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	c := cred.Claims
	if c.Subject != "alice" {
		t.Fatalf("Subject = %q, want alice", c.Subject)
	}
	assertClaims(t, store.IdentityClaims{Roles: c.Roles, Groups: c.Groups, Entitlements: c.Entitlements}, claims)
	if c.Issuer != "" || c.Audience != nil || c.Expiry != nil || c.NotBefore != nil ||
		c.IssuedAt != nil || c.ID != "" || c.Scope != "" || c.ClientID != "" {
		t.Fatalf("token claims not zero-valued: %+v", c)
	}
}

// Claims larger than 64 KiB and multi-KiB hashes must round-trip.
func testLargeValues(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	roles := make([]string, 2000)
	for i := range roles {
		roles[i] = "role-" + strconv.Itoa(i) + "-" + strings.Repeat("x", 60)
	}
	claims := store.IdentityClaims{Roles: roles}
	mustCreate(t, s, "alice", claims)
	got, err := s.GetIdentity(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	assertClaims(t, got.Claims, claims)

	hash := "h-" + strings.Repeat("y", 8192)
	mustCreatePassword(t, s, "alice", "alice@example.com", hash)
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if cred.Hash != hash {
		t.Fatalf("large hash did not round-trip: %d bytes back", len(cred.Hash))
	}
	if len(cred.Claims.Roles) != len(roles) {
		t.Fatalf("large claims did not round-trip through Lookup: %d roles", len(cred.Claims.Roles))
	}
}

func testCopyIsolation(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	want := store.IdentityClaims{
		Roles:        []string{"admin"},
		Groups:       []string{"staff"},
		Entitlements: []string{"beta"},
	}
	mutate := func(c *store.IdentityClaims, tag string) {
		c.Roles[0] = tag
		c.Groups[0] = tag
		c.Entitlements[0] = tag
	}
	claims := store.IdentityClaims{
		Roles:        []string{"admin"},
		Groups:       []string{"staff"},
		Entitlements: []string{"beta"},
	}
	created := mustCreate(t, s, "alice", claims)
	mutate(&claims, "mutated-input")
	mutate(&created.Claims, "mutated-return")
	got, err := s.GetIdentity(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	assertClaims(t, got.Claims, want)
	mutate(&got.Claims, "mutated-get")
	got, err = s.GetIdentity(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	assertClaims(t, got.Claims, want)

	replacement := store.IdentityClaims{
		Roles:        []string{"admin"},
		Groups:       []string{"staff"},
		Entitlements: []string{"beta"},
	}
	if err := s.SetIdentityClaims(ctx, "alice", replacement); err != nil {
		t.Fatalf("SetIdentityClaims: %v", err)
	}
	mutate(&replacement, "mutated-set-input")
	got, err = s.GetIdentity(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	assertClaims(t, got.Claims, want)

	mustCreatePassword(t, s, "alice", "alice@example.com", testHash(t, "secret"))
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	assertClaims(t, store.IdentityClaims{Roles: cred.Claims.Roles, Groups: cred.Claims.Groups, Entitlements: cred.Claims.Entitlements}, want)
	cred.Claims.Roles[0] = "mutated-cred"
	cred.Claims.Groups[0] = "mutated-cred"
	cred.Claims.Entitlements[0] = "mutated-cred"
	again, err := s.Lookup(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	assertClaims(t, store.IdentityClaims{Roles: again.Claims.Roles, Groups: again.Claims.Groups, Entitlements: again.Claims.Entitlements}, want)
}

func testTimestamps(t *testing.T, newStore Factory) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	current := base
	var mu sync.Mutex
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	step := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		current = current.Add(d)
	}
	s := newStore(t, now)
	ctx := context.Background()
	created := mustCreate(t, s, "alice", store.IdentityClaims{})
	if created.CreatedAt.UnixNano() != base.UnixNano() {
		t.Fatalf("CreatedAt = %v, want %v", created.CreatedAt, base)
	}
	if created.UpdatedAt.UnixNano() != base.UnixNano() {
		t.Fatalf("UpdatedAt = %v, want %v", created.UpdatedAt, base)
	}

	step(time.Minute)
	if err := s.SetIdentityStatus(ctx, "alice", store.StatusDisabled); err != nil {
		t.Fatalf("SetIdentityStatus: %v", err)
	}
	got, err := s.GetIdentity(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	if got.CreatedAt.UnixNano() != base.UnixNano() {
		t.Fatalf("CreatedAt changed on mutation: %v", got.CreatedAt)
	}
	if got.UpdatedAt.UnixNano() != base.Add(time.Minute).UnixNano() {
		t.Fatalf("UpdatedAt = %v, want the status-change instant", got.UpdatedAt)
	}

	step(time.Minute)
	if err := s.SetIdentityClaims(ctx, "alice", store.IdentityClaims{Roles: []string{"x"}}); err != nil {
		t.Fatalf("SetIdentityClaims: %v", err)
	}
	got, err = s.GetIdentity(ctx, "alice")
	if err != nil {
		t.Fatalf("GetIdentity: %v", err)
	}
	if got.UpdatedAt.UnixNano() != base.Add(2*time.Minute).UnixNano() {
		t.Fatalf("UpdatedAt = %v, want the claims-change instant", got.UpdatedAt)
	}
	if got.CreatedAt.UnixNano() != base.UnixNano() {
		t.Fatalf("SetIdentityClaims moved CreatedAt: %v", got.CreatedAt)
	}

	step(time.Minute)
	if err := s.SetIdentityStatus(ctx, "alice", store.StatusActive); err != nil {
		t.Fatalf("SetIdentityStatus: %v", err)
	}
	hash := testHash(t, "secret")
	next := testHash(t, "next")
	for _, write := range []struct {
		name string
		call func() error
	}{
		{"CreatePassword", func() error { return s.CreatePassword(ctx, "alice", "alice@example.com", hash) }},
		{"SetPassword", func() error { return s.SetPassword(ctx, "alice", "alice@example.com", hash) }},
		{"UpdateHash", func() error { return s.UpdateHash(ctx, "alice@example.com", hash, next) }},
	} {
		step(time.Minute)
		if err := write.call(); err != nil {
			t.Fatalf("%s: %v", write.name, err)
		}
		got, err = s.GetIdentity(ctx, "alice")
		if err != nil {
			t.Fatalf("GetIdentity: %v", err)
		}
		if got.UpdatedAt.UnixNano() != base.Add(3*time.Minute).UnixNano() {
			t.Fatalf("%s moved identity UpdatedAt: %v", write.name, got.UpdatedAt)
		}
		if got.CreatedAt.UnixNano() != base.UnixNano() {
			t.Fatalf("%s moved CreatedAt: %v", write.name, got.CreatedAt)
		}
	}
}

func testUpdateHashCAS(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{})
	hash := testHash(t, "secret")
	mustCreatePassword(t, s, "alice", "alice@example.com", hash)
	next := testHash(t, "next")
	if err := s.UpdateHash(ctx, "alice@example.com", hash, next); err != nil {
		t.Fatalf("UpdateHash: %v", err)
	}
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil || cred.Hash != next {
		t.Fatalf("Lookup after UpdateHash = %q, %v", cred.Hash, err)
	}
	if err := s.UpdateHash(ctx, "alice@example.com", hash, testHash(t, "stale")); !errors.Is(err, password.ErrHashChanged) {
		t.Fatalf("UpdateHash(stale) = %v, want password.ErrHashChanged", err)
	}
	if err := s.UpdateHash(ctx, "alice@example.com", next+" ", testHash(t, "padded")); !errors.Is(err, password.ErrHashChanged) {
		t.Fatalf("UpdateHash(padded oldHash) = %v, want password.ErrHashChanged", err)
	}
	flipped := strings.ToUpper(next)
	if flipped == next {
		flipped = strings.ToLower(next)
	}
	if err := s.UpdateHash(ctx, "alice@example.com", flipped, testHash(t, "flipped")); !errors.Is(err, password.ErrHashChanged) {
		t.Fatalf("UpdateHash(case-variant oldHash) = %v, want password.ErrHashChanged", err)
	}
	cred, err = s.Lookup(ctx, "alice@example.com")
	if err != nil || cred.Hash != next {
		t.Fatalf("stale UpdateHash changed the hash: %q, %v", cred.Hash, err)
	}
}

func testUpdateHashConcurrent(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustCreate(t, s, "alice", store.IdentityClaims{})
	old := testHash(t, "secret")
	mustCreatePassword(t, s, "alice", "alice@example.com", old)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.UpdateHash(ctx, "alice@example.com", old, "new-"+strings.Repeat("x", i+1))
		}()
	}
	wg.Wait()
	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, password.ErrHashChanged):
		default:
			t.Fatalf("concurrent UpdateHash[%d] = %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent UpdateHash winners = %d, want 1", winners)
	}
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	for i, err := range errs {
		if err == nil && cred.Hash != "new-"+strings.Repeat("x", i+1) {
			t.Fatalf("persisted hash %q is not the winner's", cred.Hash)
		}
	}
}

func testVerifierAuthenticate(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	v, err := password.New(password.Config{Store: s, Hasher: lightHasher()})
	if err != nil {
		t.Fatalf("password.New: %v", err)
	}
	hash, err := lightHasher().Hash("secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	mustCreate(t, s, "alice", store.IdentityClaims{Roles: []string{"admin"}})
	mustCreatePassword(t, s, "alice", "alice@example.com", hash)

	claims, err := v.Authenticate(ctx, "Alice@Example.com", "secret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if claims.Subject != "alice" || len(claims.Roles) != 1 || claims.Roles[0] != "admin" {
		t.Fatalf("Authenticate claims = %+v", claims)
	}
	if _, err := v.Authenticate(ctx, "alice@example.com", "wrong"); !errors.Is(err, password.ErrInvalidCredentials) {
		t.Fatalf("Authenticate(wrong password) = %v, want ErrInvalidCredentials", err)
	}
	if _, err := v.Authenticate(ctx, "ghost@example.com", "secret"); !errors.Is(err, password.ErrInvalidCredentials) {
		t.Fatalf("Authenticate(unknown email) = %v, want ErrInvalidCredentials", err)
	}
	if _, err := v.Authenticate(ctx, "   ", "secret"); !errors.Is(err, password.ErrInvalidCredentials) {
		t.Fatalf("Authenticate(blank email) = %v, want ErrInvalidCredentials", err)
	}
	if err := s.SetIdentityStatus(ctx, "alice", store.StatusDisabled); err != nil {
		t.Fatalf("SetIdentityStatus: %v", err)
	}
	if _, err := v.Authenticate(ctx, "alice@example.com", "secret"); !errors.Is(err, password.ErrInvalidCredentials) {
		t.Fatalf("Authenticate(disabled) = %v, want ErrInvalidCredentials", err)
	}
}

func testVerifierRehash(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	legacy := password.Argon2id{Time: 1, MemoryKiB: 8192}
	current := password.Argon2id{Time: 2, MemoryKiB: 8192}
	hash, err := legacy.Hash("secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	mustCreate(t, s, "alice", store.IdentityClaims{})
	mustCreatePassword(t, s, "alice", "alice@example.com", hash)
	v, err := password.New(password.Config{Store: s, Hasher: current})
	if err != nil {
		t.Fatalf("password.New: %v", err)
	}
	if _, err := v.Authenticate(ctx, "alice@example.com", "secret"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if cred.Hash == hash {
		t.Fatalf("hash not upgraded through the Rehasher seam")
	}
	if current.NeedsRehash(cred.Hash) {
		t.Fatalf("upgraded hash %q still needs rehash", cred.Hash)
	}
}

// raceStore changes the credential between verifier lookup and CAS update.
type raceStore struct {
	store.Store
	swap func()
	once sync.Once
}

func (r *raceStore) Lookup(ctx context.Context, email string) (password.Credential, error) {
	cred, err := r.Store.Lookup(ctx, email)
	if err == nil {
		r.once.Do(r.swap)
	}
	return cred, err
}

func testVerifierRehashLosesRace(t *testing.T, newStore Factory) {
	s := newStore(t, nil)
	ctx := context.Background()
	legacy := password.Argon2id{Time: 1, MemoryKiB: 8192}
	current := password.Argon2id{Time: 2, MemoryKiB: 8192}
	hash, err := legacy.Hash("secret")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	replacement, err := current.Hash("changed")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	mustCreate(t, s, "alice", store.IdentityClaims{})
	mustCreatePassword(t, s, "alice", "alice@example.com", hash)
	racing := &raceStore{Store: s, swap: func() {
		if err := s.SetPassword(ctx, "alice", "alice@example.com", replacement); err != nil {
			t.Errorf("SetPassword(race): %v", err)
		}
	}}
	v, err := password.New(password.Config{Store: racing, Hasher: current})
	if err != nil {
		t.Fatalf("password.New: %v", err)
	}
	claims, err := v.Authenticate(ctx, "alice@example.com", "secret")
	if err != nil {
		t.Fatalf("Authenticate through lost rehash race: %v", err)
	}
	if claims.Subject != "alice" {
		t.Fatalf("Authenticate Subject = %q, want alice", claims.Subject)
	}
	cred, err := s.Lookup(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if cred.Hash != replacement {
		t.Fatalf("rehash overwrote the concurrent password change: %q", cred.Hash)
	}
}

func testHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := lightHasher().Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	return hash
}

func assertClaims(t *testing.T, got, want store.IdentityClaims) {
	t.Helper()
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	if !eq(got.Roles, want.Roles) || !eq(got.Groups, want.Groups) || !eq(got.Entitlements, want.Entitlements) {
		t.Fatalf("claims = %+v, want %+v", got, want)
	}
}
