package password

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/authn"
)

type countingHasher struct {
	Argon2id
	compares atomic.Int64
	hashes   atomic.Int64
	inflight atomic.Int64
	peak     atomic.Int64
	block    chan struct{}
	entered  chan struct{}
}

func newCountingHasher() *countingHasher {
	return &countingHasher{Argon2id: Argon2id{Time: 1, MemoryKiB: 8192}}
}

func (h *countingHasher) track() func() {
	n := h.inflight.Add(1)
	for {
		p := h.peak.Load()
		if n <= p || h.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if h.entered != nil {
		select {
		case h.entered <- struct{}{}:
		default:
		}
	}
	if h.block != nil {
		<-h.block
	}
	return func() { h.inflight.Add(-1) }
}

func (h *countingHasher) Compare(encoded, password string) error {
	defer h.track()()
	h.compares.Add(1)
	return h.Argon2id.Compare(encoded, password)
}

func (h *countingHasher) Hash(password string) (string, error) {
	defer h.track()()
	h.hashes.Add(1)
	return h.Argon2id.Hash(password)
}

func seeded(t *testing.T, h Hasher) *MemoryStore {
	t.Helper()
	store := NewMemoryStore()
	encoded, err := h.Hash("open sesame")
	if err != nil {
		t.Fatal(err)
	}
	store.Put("alice@example.com", Credential{
		Hash:   encoded,
		Claims: authn.ClaimSet{Subject: "alice", Roles: []string{"admin"}},
	})
	return store
}

func newVerifier(t *testing.T, cfg Config) *Verifier {
	t.Helper()
	v, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAuthenticateSuccess(t *testing.T) {
	h := newCountingHasher()
	v := newVerifier(t, Config{Store: seeded(t, h), Hasher: h})
	c, err := v.Authenticate(t.Context(), "alice@example.com", "open sesame")
	if err != nil || c == nil {
		t.Fatalf("Authenticate = %v, %v", c, err)
	}
	if c.Subject != "alice" || len(c.Roles) != 1 || c.Roles[0] != "admin" {
		t.Fatalf("claims = %+v", c)
	}
}

func TestUniformFailure(t *testing.T) {
	h := newCountingHasher()
	v := newVerifier(t, Config{Store: seeded(t, h), Hasher: h})
	tests := []struct {
		name     string
		email    string
		password string
	}{
		{"unknown email", "nobody@example.com", "open sesame"},
		{"wrong password", "alice@example.com", "wrong"},
		{"empty password", "alice@example.com", ""},
		{"empty email", "", "open sesame"},
		{"oversized password", "alice@example.com", strings.Repeat("a", MaxPasswordLen+1)},
		{"oversized email", strings.Repeat("a", MaxEmailLen-11) + "@example.com", "open sesame"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := v.Authenticate(t.Context(), tt.email, tt.password)
			if c != nil || !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("got %v, %v; want nil, ErrInvalidCredentials", c, err)
			}
		})
	}
}

func TestUnknownEmailBurnsDecoyCompare(t *testing.T) {
	h := newCountingHasher()
	v := newVerifier(t, Config{Store: seeded(t, h), Hasher: h})
	base := h.compares.Load()
	if _, err := v.Authenticate(t.Context(), "nobody@example.com", "pw"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal(err)
	}
	if got := h.compares.Load() - base; got != 1 {
		t.Fatalf("unknown-email path ran %d compares, want exactly 1 (the decoy)", got)
	}
	// Oversized inputs must not reach the hasher or store.
	counting := &countingStore{}
	vb := newVerifier(t, Config{Store: counting, Hasher: h})
	base = h.compares.Load()
	vb.Authenticate(t.Context(), "alice@example.com", strings.Repeat("a", MaxPasswordLen+1))
	vb.Authenticate(t.Context(), "", "pw")
	vb.Authenticate(t.Context(), strings.Repeat("a", MaxEmailLen+1), "pw")
	if got := h.compares.Load() - base; got != 0 {
		t.Fatalf("input-bound failures ran %d compares, want 0", got)
	}
	if n := counting.lookups.Load(); n != 0 {
		t.Fatalf("input-bound failures reached the store %d times, want 0", n)
	}
}

type countingStore struct{ lookups atomic.Int64 }

func (s *countingStore) Lookup(context.Context, string) (Credential, error) {
	s.lookups.Add(1)
	return Credential{}, ErrNotFound
}

func TestNormalizeEmail(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Alice@Example.COM", "alice@example.com"},
		{"  alice@example.com	", "alice@example.com"},
		{"alice@example.com", "alice@example.com"},
		{" MIXED@Case.Org ", "mixed@case.org"},
	}
	for _, tt := range tests {
		if got := NormalizeEmail(tt.in); got != tt.want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestDecoyPathCostsRealWork pins comparable work for unknown and known emails.
func TestDecoyPathCostsRealWork(t *testing.T) {
	h := Argon2id{Time: 1, MemoryKiB: 8192}
	v := newVerifier(t, Config{Store: seeded(t, h), Hasher: h})

	perOp := func(email string) uint64 {
		const runs = 4
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for range runs {
			if _, err := v.Authenticate(t.Context(), email, "wrong"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatal(err)
			}
		}
		runtime.ReadMemStats(&after)
		return (after.TotalAlloc - before.TotalAlloc) / runs
	}
	known := perOp("alice@example.com")
	unknown := perOp("nobody@example.com")
	// Both paths run an 8 MiB derivation; allow measurement variance.
	if unknown < known*3/4 {
		t.Fatalf("unknown-email path allocated %d bytes vs %d for wrong password", unknown, known)
	}
	if unknown < 8<<20 {
		t.Fatalf("unknown-email path allocated %d bytes, below one derivation's memory", unknown)
	}
}

type erroringStore struct{ err error }

func (s erroringStore) Lookup(context.Context, string) (Credential, error) {
	return Credential{}, s.err
}

func TestStoreErrorsSurface(t *testing.T) {
	h := newCountingHasher()
	infra := errors.New("store down")
	v := newVerifier(t, Config{Store: erroringStore{err: infra}, Hasher: h})
	if _, err := v.Authenticate(t.Context(), "alice@example.com", "pw"); !errors.Is(err, infra) || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("infra error = %v, want the store error, never ErrInvalidCredentials", err)
	}
}

func TestMalformedStoredHashSurfaces(t *testing.T) {
	h := newCountingHasher()
	store := NewMemoryStore()
	store.Put("bob@example.com", Credential{Hash: "corrupt", Claims: authn.ClaimSet{Subject: "bob"}})
	v := newVerifier(t, Config{Store: store, Hasher: h})
	_, err := v.Authenticate(t.Context(), "bob@example.com", "pw")
	if !errors.Is(err, ErrInvalidHash) || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("corrupt hash = %v, want ErrInvalidHash surfaced, never ErrInvalidCredentials", err)
	}
}

func TestRehashOnLogin(t *testing.T) {
	old := Argon2id{Time: 1, MemoryKiB: 8192}
	encoded, err := old.Hash("open sesame")
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	store.Put("alice@example.com", Credential{Hash: encoded, Claims: authn.ClaimSet{Subject: "alice"}})
	current := Argon2id{Time: 1, MemoryKiB: 16384}
	v := newVerifier(t, Config{Store: store, Hasher: current})

	if _, err := v.Authenticate(t.Context(), "alice@example.com", "open sesame"); err != nil {
		t.Fatal(err)
	}
	cred, err := store.Lookup(t.Context(), "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Hash == encoded {
		t.Fatal("stale hash not rewritten")
	}
	if !strings.HasPrefix(cred.Hash, "$argon2id$v=19$m=16384,t=1,p=1$") {
		t.Fatalf("rewritten hash has wrong profile: %s", cred.Hash)
	}
	if err := current.Compare(cred.Hash, "open sesame"); err != nil {
		t.Fatalf("rewritten hash does not verify: %v", err)
	}

	// A current hash must not be rewritten.
	if _, err := v.Authenticate(t.Context(), "alice@example.com", "open sesame"); err != nil {
		t.Fatal(err)
	}
	again, _ := store.Lookup(t.Context(), "alice@example.com")
	if again.Hash != cred.Hash {
		t.Fatal("current hash rewritten on second login")
	}
}

// raceStore opens a reset window between Lookup and UpdateHash.
type raceStore struct {
	*MemoryStore
	onLookup func()
}

func (s *raceStore) Lookup(ctx context.Context, email string) (Credential, error) {
	cred, err := s.MemoryStore.Lookup(ctx, email)
	if s.onLookup != nil {
		s.onLookup()
	}
	return cred, err
}

func TestRehashLosesToConcurrentReset(t *testing.T) {
	old := Argon2id{Time: 1, MemoryKiB: 8192}
	oldHash, err := old.Hash("open sesame")
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemoryStore()
	mem.Put("alice@example.com", Credential{Hash: oldHash, Claims: authn.ClaimSet{Subject: "alice"}})
	current := Argon2id{Time: 1, MemoryKiB: 16384}

	resetHash, err := current.Hash("brand new password")
	if err != nil {
		t.Fatal(err)
	}
	store := &raceStore{MemoryStore: mem}
	store.onLookup = func() {
		mem.Put("alice@example.com", Credential{Hash: resetHash, Claims: authn.ClaimSet{Subject: "alice"}})
	}
	v := newVerifier(t, Config{Store: store, Hasher: current})

	// A password verified before the reset succeeds, but its rehash loses the CAS.
	if _, err := v.Authenticate(t.Context(), "alice@example.com", "open sesame"); err != nil {
		t.Fatal(err)
	}
	cred, _ := mem.Lookup(t.Context(), "alice@example.com")
	if cred.Hash != resetHash {
		t.Fatalf("concurrent reset undone: stored hash is %q, want the reset hash", cred.Hash[:40])
	}
}

type readOnlyStore struct{ inner *MemoryStore }

func (s readOnlyStore) Lookup(ctx context.Context, email string) (Credential, error) {
	return s.inner.Lookup(ctx, email)
}

func TestReadOnlyStoreUntouched(t *testing.T) {
	old := Argon2id{Time: 1, MemoryKiB: 8192}
	encoded, err := old.Hash("open sesame")
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemoryStore()
	mem.Put("alice@example.com", Credential{Hash: encoded, Claims: authn.ClaimSet{Subject: "alice"}})
	v := newVerifier(t, Config{Store: readOnlyStore{inner: mem}, Hasher: Argon2id{Time: 1, MemoryKiB: 16384}})
	if _, err := v.Authenticate(t.Context(), "alice@example.com", "open sesame"); err != nil {
		t.Fatal(err)
	}
	cred, _ := mem.Lookup(t.Context(), "alice@example.com")
	if cred.Hash != encoded {
		t.Fatal("read-only store was written")
	}
}

type failingRehashStore struct{ *MemoryStore }

var errRefused = errors.New("write refused")

func (s failingRehashStore) UpdateHash(context.Context, string, string, string) error {
	return errRefused
}

func TestFailedRehashLogsAndSucceeds(t *testing.T) {
	old := Argon2id{Time: 1, MemoryKiB: 8192}
	encoded, err := old.Hash("open sesame")
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemoryStore()
	mem.Put("alice@example.com", Credential{Hash: encoded, Claims: authn.ClaimSet{Subject: "alice"}})
	var buf bytes.Buffer
	v := newVerifier(t, Config{
		Store:  failingRehashStore{mem},
		Hasher: Argon2id{Time: 1, MemoryKiB: 16384},
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	})
	if _, err := v.Authenticate(t.Context(), "alice@example.com", "open sesame"); err != nil {
		t.Fatalf("failed rehash failed the login: %v", err)
	}
	if log := buf.String(); !strings.Contains(log, "rehash") || !strings.Contains(log, "write refused") {
		t.Fatalf("rehash failure not logged with the cause: %q", log)
	}
}

func TestHashChangedSkipsSilently(t *testing.T) {
	old := Argon2id{Time: 1, MemoryKiB: 8192}
	encoded, err := old.Hash("open sesame")
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemoryStore()
	mem.Put("alice@example.com", Credential{Hash: encoded, Claims: authn.ClaimSet{Subject: "alice"}})
	var buf bytes.Buffer
	store := &raceStore{MemoryStore: mem}
	replacement, err := old.Hash("open sesame")
	if err != nil {
		t.Fatal(err)
	}
	store.onLookup = func() {
		mem.Put("alice@example.com", Credential{Hash: replacement, Claims: authn.ClaimSet{Subject: "alice"}})
	}
	v := newVerifier(t, Config{
		Store:  store,
		Hasher: Argon2id{Time: 1, MemoryKiB: 16384},
		Logger: slog.New(slog.NewTextHandler(&buf, nil)),
	})
	if _, err := v.Authenticate(t.Context(), "alice@example.com", "open sesame"); err != nil {
		t.Fatal(err)
	}
	if log := buf.String(); strings.Contains(log, "rehash") {
		t.Fatalf("CAS miss was logged, want silent skip: %q", log)
	}
}

// rawStore exposes shared claim storage to test the verifier's clone boundary.
type rawStore struct{ cred Credential }

func (s *rawStore) Lookup(context.Context, string) (Credential, error) {
	return s.cred, nil
}

func TestClaimsMutationIsolated(t *testing.T) {
	h := newCountingHasher()
	encoded, err := h.Hash("open sesame")
	if err != nil {
		t.Fatal(err)
	}
	store := &rawStore{cred: Credential{
		Hash: encoded,
		Claims: authn.ClaimSet{
			Subject:  "alice",
			Audience: authn.Audience{"api"},
			IssuedAt: authn.NewNumericDate(time.Unix(1700000000, 0)),
			Roles:    []string{"admin"},
		},
	}}
	v := newVerifier(t, Config{Store: store, Hasher: h})
	c, err := v.Authenticate(t.Context(), "alice@example.com", "open sesame")
	if err != nil {
		t.Fatal(err)
	}
	c.Roles[0] = "evil"
	c.Audience[0] = "evil"
	*c.IssuedAt = authn.NumericDate(1)
	if store.cred.Claims.Roles[0] != "admin" || store.cred.Claims.Audience[0] != "api" ||
		store.cred.Claims.IssuedAt.Time().Unix() != 1700000000 {
		t.Fatal("mutating returned claims reached the stored credential")
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := New(Config{Store: NewMemoryStore(), MaxConcurrent: -1}); err == nil {
		t.Error("negative MaxConcurrent accepted")
	}
	if _, err := New(Config{Store: NewMemoryStore(), Hasher: Argon2id{Time: 17}}); err == nil {
		t.Error("self-check passed an out-of-bounds hasher")
	}
	if _, err := New(Config{Store: NewMemoryStore(), Hasher: brokenHasher{}}); err == nil {
		t.Error("self-check passed a broken hasher")
	}
	if _, err := New(Config{Store: NewMemoryStore(), Hasher: alwaysYesHasher{}}); err == nil {
		t.Error("self-check passed an always-accepting hasher")
	}
}

func TestMaxConcurrentBoundsHashing(t *testing.T) {
	h := newCountingHasher()
	store := seeded(t, h)
	v := newVerifier(t, Config{Store: store, Hasher: h, MaxConcurrent: 2})
	h.block = make(chan struct{})
	h.peak.Store(0)

	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v.Authenticate(context.Background(), fmt.Sprintf("u%d@example.com", i), "pw")
		}(i)
	}
	// Allow calls to queue behind the hashing slots.
	time.Sleep(100 * time.Millisecond)
	close(h.block)
	wg.Wait()
	if peak := h.peak.Load(); peak > 2 {
		t.Fatalf("observed %d concurrent hash operations, cap 2", peak)
	}
}

func TestMaxConcurrentDefault(t *testing.T) {
	v := newVerifier(t, Config{Store: NewMemoryStore()})
	if got := cap(v.sem); got != 4 {
		t.Fatalf("default semaphore capacity = %d, want 4", got)
	}
}

// TestRehashSharesSemaphore pins one concurrency bound across compare and rehash.
func TestRehashSharesSemaphore(t *testing.T) {
	old := Argon2id{Time: 1, MemoryKiB: 8192}
	h := newCountingHasher()
	h.Argon2id = Argon2id{Time: 1, MemoryKiB: 16384}
	mem := NewMemoryStore()
	for i := range 3 {
		encoded, err := old.Hash("pw")
		if err != nil {
			t.Fatal(err)
		}
		mem.Put(fmt.Sprintf("u%d@example.com", i), Credential{Hash: encoded, Claims: authn.ClaimSet{Subject: "u"}})
	}
	v := newVerifier(t, Config{Store: mem, Hasher: h, MaxConcurrent: 1})
	h.peak.Store(0)
	baseline := h.hashes.Load() // New already hashed (self-check, decoy)

	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := v.Authenticate(context.Background(), fmt.Sprintf("u%d@example.com", i), "pw"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if got := h.hashes.Load() - baseline; got != 3 {
		t.Fatalf("rehash ran %d hashes after construction, want 3", got)
	}
	if peak := h.peak.Load(); peak > 1 {
		t.Fatalf("observed %d concurrent operations across compare+rehash, cap 1", peak)
	}
}

func TestQueuedCancellation(t *testing.T) {
	h := newCountingHasher()
	store := seeded(t, h)
	v := newVerifier(t, Config{Store: store, Hasher: h, MaxConcurrent: 1})
	h.block = make(chan struct{})
	h.entered = make(chan struct{}, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		v.Authenticate(context.Background(), "alice@example.com", "open sesame")
	}()
	// The first call holds the only hashing slot.
	<-h.entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := v.Authenticate(ctx, "alice@example.com", "open sesame")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("queued cancellation = %v, want context.Canceled, never ErrInvalidCredentials", err)
	}
	close(h.block)
	wg.Wait()
}

var _ Store = (*MemoryStore)(nil)
var _ Rehasher = (*MemoryStore)(nil)
