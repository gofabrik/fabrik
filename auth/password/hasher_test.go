package password_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/auth/password"
)

// captureSlog swaps in a slog handler that writes to buf for the
// duration of fn. Returns buf's contents after fn returns. The Warn
// level is the lowest enabled so callers can assert on Warn (and
// lower-noise levels stay silent).
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	prev := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fn()
	return buf.String()
}

// TestBcryptHasher_Verify_CorrectPassword pins the happy path: a
// real hash matched against the right plaintext returns true silently.
func TestBcryptHasher_Verify_CorrectPassword(t *testing.T) {
	h := password.BcryptHasher{Cost: 4}
	hash, err := h.Hash("correct-password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	out := captureSlog(t, func() {
		if !h.Verify("correct-password", hash) {
			t.Error("Verify returned false on correct password")
		}
	})
	if out != "" {
		t.Errorf("Verify on correct password logged %q, want silence", out)
	}
}

// TestBcryptHasher_Verify_WrongPassword pins that the expected
// negative case (bcrypt.ErrMismatchedHashAndPassword) is silent.
// Logging every failed login would dwarf real diagnostics and would
// be a free DoS amplifier under brute-force traffic.
func TestBcryptHasher_Verify_WrongPassword(t *testing.T) {
	h := password.BcryptHasher{Cost: 4}
	hash, err := h.Hash("correct-password")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	out := captureSlog(t, func() {
		if h.Verify("WRONG", hash) {
			t.Error("Verify returned true on wrong password")
		}
	})
	if out != "" {
		t.Errorf("Verify on wrong password logged %q, want silence - only non-mismatch errors should log", out)
	}
}

// TestBcryptHasher_Verify_CorruptHash_LogsWarning pins the
// diagnostic path: a malformed/unsupported hash (anything that
// trips bcrypt with an error other than ErrMismatchedHashAndPassword)
// must surface via slog.Warn so ops can spot the broken row.
//
// Without the warning, every login attempt for the affected account
// would silently 401 forever with no trail.
func TestBcryptHasher_Verify_CorruptHash_LogsWarning(t *testing.T) {
	h := password.BcryptHasher{Cost: 4}

	// Too-short string trips bcrypt.ErrHashTooShort, a representative
	// non-mismatch error. Could equally be any malformed bcrypt
	// payload - the contract is "any non-mismatch logs".
	corrupt := []byte("not-a-real-hash")

	out := captureSlog(t, func() {
		if h.Verify("any-password", corrupt) {
			t.Error("Verify returned true against a corrupt hash")
		}
	})
	if !strings.Contains(out, "WARN") {
		t.Errorf("log output = %q, want a WARN level entry", out)
	}
	if !strings.Contains(out, "bcrypt") {
		t.Errorf("log output = %q, want 'bcrypt' so the source is obvious", out)
	}
	// Must never log the plaintext.
	if strings.Contains(out, "any-password") {
		t.Errorf("log output = %q LEAKS the plaintext password - must never include credentials", out)
	}
}

// TestBcryptHasher_Verify_VersionMismatch_LogsWarning is a second
// pin on the diagnostic path using a different bcrypt failure mode
// (version byte the binary doesn't support). Ensures the warning
// isn't tied to the specific length-check error path.
func TestBcryptHasher_Verify_VersionMismatch_LogsWarning(t *testing.T) {
	h := password.BcryptHasher{Cost: 4}

	// A bcrypt-shaped string with a version prefix bcrypt won't
	// accept. The point is to trip a non-mismatch failure inside
	// CompareHashAndPassword.
	versionWrong := []byte("$9z$04$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")

	out := captureSlog(t, func() {
		if h.Verify("p", versionWrong) {
			t.Error("Verify returned true against a bogus version hash")
		}
	})
	if !strings.Contains(out, "WARN") {
		t.Errorf("log output = %q, want a WARN level entry for the non-mismatch error", out)
	}
}
