package password

import (
	"encoding/base64"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestHashCompareRoundTrip(t *testing.T) {
	var h Argon2id
	encoded, err := h.Hash("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("default profile not encoded: %s", encoded)
	}
	if err := h.Compare(encoded, "correct horse"); err != nil {
		t.Fatalf("match = %v", err)
	}
	if err := h.Compare(encoded, "wrong"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("mismatch = %v, want ErrMismatch", err)
	}
}

// TestKnownAnswer pins compatibility with hashes emitted by this package.
func TestKnownAnswer(t *testing.T) {
	const encoded = "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$hQheIKtfw7ihXSY5dgoOKBJRDbzpITyAn2cijbgvJCY"
	var h Argon2id
	if err := h.Compare(encoded, "known answer"); err != nil {
		t.Fatalf("known vector rejected: %v", err)
	}
	if err := h.Compare(encoded, "known answer!"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("wrong password on vector = %v", err)
	}
}

func TestSaltRandomness(t *testing.T) {
	var h Argon2id
	a, err := h.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two hashes of one password are identical")
	}
}

func TestCompareAcceptsOtherProfiles(t *testing.T) {
	old := Argon2id{Time: 1, MemoryKiB: 8192, Parallelism: 2}
	encoded, err := old.Hash("upgrade me")
	if err != nil {
		t.Fatal(err)
	}
	var current Argon2id
	if err := current.Compare(encoded, "upgrade me"); err != nil {
		t.Fatalf("old-profile hash rejected: %v", err)
	}
	if !current.NeedsRehash(encoded) {
		t.Fatal("old-profile hash not flagged for rehash")
	}
}

func TestNeedsRehash(t *testing.T) {
	var h Argon2id
	current, err := h.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		encoded string
		want    bool
	}{
		{"current profile", current, false},
		{"stronger time", "$argon2id$v=19$m=19456,t=3,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", true},
		{"weaker time", "$argon2id$v=19$m=19456,t=1,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", true},
		{"stronger memory", "$argon2id$v=19$m=65536,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", true},
		{"weaker memory", "$argon2id$v=19$m=8192,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", true},
		{"different lanes", "$argon2id$v=19$m=19456,t=2,p=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", true},
		{"noncanonical salt length", "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", true},
		{"noncanonical key length", "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAA", true},
		{"unparseable", "not a hash", true},
		{"empty", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.NeedsRehash(tt.encoded); got != tt.want {
				t.Errorf("NeedsRehash = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMalformedHashes(t *testing.T) {
	// Malformed hashes must report ErrInvalidHash, not ErrMismatch.
	valid := "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	tests := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"no dollar", "argon2id"},
		{"wrong algorithm", strings.Replace(valid, "argon2id", "argon2i", 1)},
		{"bcrypt", "$2a$10$abcdefghijklmnopqrstuv"},
		{"wrong version", strings.Replace(valid, "v=19", "v=16", 1)},
		{"version not numeric", strings.Replace(valid, "v=19", "v=x", 1)},
		{"missing version", "$argon2id$m=19456,t=2,p=1$AAAA$AAAA"},
		{"four segments", "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA"},
		{"seven segments", valid + "$extra"},
		{"two params", "$argon2id$v=19$m=19456,t=2$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"four params", "$argon2id$v=19$m=19456,t=2,p=1,x=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"reordered params", "$argon2id$v=19$t=2,m=19456,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"negative memory", strings.Replace(valid, "m=19456", "m=-1", 1)},
		{"memory not numeric", strings.Replace(valid, "m=19456", "m=lots", 1)},
		{"memory overflow", strings.Replace(valid, "m=19456", "m=4294967296", 1)},
		{"bad salt base64", "$argon2id$v=19$m=19456,t=2,p=1$!!!!$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"bad key base64", "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$!!!!"},
		{"salt too short", "$argon2id$v=19$m=19456,t=2,p=1$AAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"key too short", "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAA"},
		{"oversized encoded", "$argon2id$v=19$m=19456,t=2,p=1$" + strings.Repeat("A", 4096)},
		{"padded base64", "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA==$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
	}
	var h Argon2id
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.Compare(tt.encoded, "pw")
			if err == nil {
				t.Fatal("malformed hash accepted")
			}
			if errors.Is(err, ErrMismatch) {
				t.Fatalf("parse failure reported as ErrMismatch: %v", err)
			}
			if !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("parse failure does not wrap ErrInvalidHash: %v", err)
			}
		})
	}
}

func TestStoredHashBounds(t *testing.T) {
	mk := func(m, tc, p string) string {
		return "$argon2id$v=19$m=" + m + ",t=" + tc + ",p=" + p +
			"$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	}
	var h Argon2id
	accepts := []struct {
		name    string
		encoded string
	}{
		{"floor", mk("8192", "1", "1")},
		{"memory ceiling", mk("131072", "1", "1")},
		{"time ceiling", mk("8192", "16", "1")},
		{"lanes ceiling", mk("8192", "1", "16")},
	}
	for _, tt := range accepts {
		t.Run("accept/"+tt.name, func(t *testing.T) {
			if err := h.Compare(tt.encoded, "pw"); !errors.Is(err, ErrMismatch) {
				t.Fatalf("in-bounds hash = %v, want ErrMismatch (accepted, wrong pw)", err)
			}
		})
	}
	rejects := []struct {
		name    string
		encoded string
	}{
		{"memory below", mk("8191", "2", "1")},
		{"memory above", mk("131073", "2", "1")},
		{"memory zero", mk("0", "2", "1")},
		{"time below", mk("19456", "0", "1")},
		{"time above", mk("19456", "17", "1")},
		{"lanes zero", mk("19456", "2", "0")},
		{"lanes above", mk("19456", "2", "17")},
	}
	for _, tt := range rejects {
		t.Run("reject/"+tt.name, func(t *testing.T) {
			err := h.Compare(tt.encoded, "pw")
			if !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("out-of-bounds hash = %v, want ErrInvalidHash", err)
			}
		})
	}
}

func TestProfileDefaultsAndBounds(t *testing.T) {
	partials := []struct {
		name string
		h    Argon2id
		want string
	}{
		{"memory only", Argon2id{MemoryKiB: 65536}, "$argon2id$v=19$m=65536,t=2,p=1$"},
		{"time only", Argon2id{Time: 3}, "$argon2id$v=19$m=19456,t=3,p=1$"},
		{"lanes only", Argon2id{Parallelism: 2}, "$argon2id$v=19$m=19456,t=2,p=2$"},
	}
	for _, tt := range partials {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := tt.h.Hash("pw")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(encoded, tt.want) {
				t.Fatalf("partial profile did not default remaining fields: %s", encoded)
			}
		})
	}

	bad := []struct {
		name string
		h    Argon2id
	}{
		{"time above", Argon2id{Time: 17}},
		{"memory below", Argon2id{MemoryKiB: 8191}},
		{"memory above", Argon2id{MemoryKiB: 131073}},
		{"lanes above", Argon2id{Parallelism: 17}},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.h.Hash("pw"); err == nil {
				t.Error("out-of-range profile hashed")
			}
			if err := tt.h.Compare("$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "pw"); err == nil || errors.Is(err, ErrMismatch) {
				t.Error("out-of-range profile compared")
			}
		})
	}

	edges := []Argon2id{
		{Time: 1, MemoryKiB: 8192, Parallelism: 1},
		{Time: 16, MemoryKiB: 8192, Parallelism: 1},
		{Time: 1, MemoryKiB: 131072, Parallelism: 1},
		{Time: 1, MemoryKiB: 8192, Parallelism: 16},
	}
	for _, h := range edges {
		if _, err := h.Hash("pw"); err != nil {
			t.Errorf("endpoint profile %+v rejected: %v", h, err)
		}
	}
}

func TestHashOutputStructure(t *testing.T) {
	var h Argon2id
	encoded, err := h.Hash("structure")
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" {
		t.Fatalf("want 6 dollar-delimited fields, got %d: %s", len(fields), encoded)
	}
	if fields[1] != "argon2id" || fields[2] != "v=19" || fields[3] != "m=19456,t=2,p=1" {
		t.Fatalf("header segments wrong: %s", encoded)
	}
	if strings.ContainsAny(fields[4]+fields[5], "=") {
		t.Fatalf("padded base64 in output: %s", encoded)
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil || len(salt) != 16 {
		t.Fatalf("salt = %d bytes, %v; want 16 raw-std bytes", len(salt), err)
	}
	key, err := base64.RawStdEncoding.DecodeString(fields[5])
	if err != nil || len(key) != 32 {
		t.Fatalf("key = %d bytes, %v; want 32 raw-std bytes", len(key), err)
	}
}

type brokenHasher struct{ Argon2id }

func (brokenHasher) Compare(string, string) error { return ErrMismatch }

type alwaysYesHasher struct{ Argon2id }

func (alwaysYesHasher) Compare(string, string) error { return nil }

// wrongErrHasher reports a non-ErrMismatch error for a wrong password.
type wrongErrHasher struct{ Argon2id }

func (h wrongErrHasher) Compare(encoded, password string) error {
	err := h.Argon2id.Compare(encoded, password)
	if errors.Is(err, ErrMismatch) {
		return errors.New("computation failed")
	}
	return err
}

func TestSelfCheck(t *testing.T) {
	if err := selfCheck(Argon2id{}); err != nil {
		t.Fatalf("healthy hasher failed self-check: %v", err)
	}
	if err := selfCheck(brokenHasher{}); err == nil {
		t.Fatal("broken hasher passed self-check")
	}
	if err := selfCheck(alwaysYesHasher{}); err == nil {
		t.Fatal("always-accepting hasher passed self-check")
	}
	if err := selfCheck(wrongErrHasher{}); err == nil {
		t.Fatal("non-ErrMismatch-rejecting hasher passed self-check")
	}
	// Profile validation must precede the probe allocation.
	if err := selfCheck(Argon2id{MemoryKiB: 131073}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("out-of-bounds hasher self-check = %v, want a bounds error", err)
	}
}

// TestRejectionAllocationBounded pins cheap rejection of oversized input and
// out-of-bounds memory claims.
func TestRejectionAllocationBounded(t *testing.T) {
	var h Argon2id
	hostile := []string{
		strings.Repeat("$", 1<<20),
		"$argon2id$v=19$m=131073,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	const runs = 16
	const ceiling = 32 << 10
	for _, encoded := range hostile {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for range runs {
			if err := h.Compare(encoded, "pw"); !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("hostile input = %v, want ErrInvalidHash", err)
			}
		}
		runtime.ReadMemStats(&after)
		perOp := (after.TotalAlloc - before.TotalAlloc) / runs
		if perOp > ceiling {
			t.Errorf("rejection of %d-byte input allocated %d bytes per attempt, ceiling %d", len(encoded), perOp, ceiling)
		}
	}
}

func TestMaxPasswordLen(t *testing.T) {
	var h Argon2id
	ok := strings.Repeat("a", MaxPasswordLen)
	encoded, err := h.Hash(ok)
	if err != nil {
		t.Fatalf("length %d rejected: %v", MaxPasswordLen, err)
	}
	if err := h.Compare(encoded, ok); err != nil {
		t.Fatalf("compare at limit: %v", err)
	}
	long := ok + "a"
	if _, err := h.Hash(long); err == nil {
		t.Error("oversized password hashed")
	}
	if err := h.Compare(encoded, long); err == nil || errors.Is(err, ErrMismatch) {
		t.Errorf("oversized compare = %v, want a non-mismatch error", err)
	}
}

var _ Hasher = Argon2id{}
