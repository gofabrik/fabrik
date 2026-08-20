// Package password verifies email/password credentials and returns claims.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// MaxPasswordLen bounds password inputs before hashing.
const MaxPasswordLen = 512

var (
	// ErrMismatch reports a password mismatch for a well-formed hash.
	ErrMismatch = errors.New("password: hash mismatch")

	// ErrInvalidHash reports a malformed or out-of-bounds Argon2id PHC string.
	ErrInvalidHash = errors.New("password: invalid encoded hash")
)

// Hasher hashes and verifies passwords. Compare returns ErrMismatch for a
// well-formed non-match and wraps ErrInvalidHash for malformed input.
// NeedsRehash reports whether an encoded hash should be replaced.
type Hasher interface {
	Hash(password string) (string, error)
	Compare(encoded, password string) error
	NeedsRehash(encoded string) bool
}

// Zero-valued Argon2id fields resolve independently to these OWASP defaults.
const (
	defaultTime      = 2
	defaultMemoryKiB = 19456
	defaultLanes     = 1
	saltLen          = 16
	keyLen           = 32
)

// Profiles and stored hashes must satisfy these bounds before Argon2 allocates.
const (
	minTime      = 1
	maxTime      = 16
	minMemoryKiB = 8192
	maxMemoryKiB = 131072
	minLanes     = 1
	maxLanes     = 16
	minSaltLen   = 8
	maxSaltLen   = 64
	minKeyLen    = 16
	maxKeyLen    = 64
)

const phcVersion = 19

// maxEncodedLen includes the largest legal salt and key plus parameter slack.
const maxEncodedLen = len("$argon2id$v=19$m=131072,t=16,p=16$") + 2*((maxSaltLen*8+5)/6) + 1 + 16

var b64 = base64.RawStdEncoding

// Argon2id is the default Hasher. Zero fields use OWASP defaults, and resolved
// profiles must lie within the package bounds.
type Argon2id struct {
	Time        uint32
	MemoryKiB   uint32
	Parallelism uint8
}

type profile struct {
	time   uint32
	memory uint32
	lanes  uint8
}

func (a Argon2id) resolve() (profile, error) {
	p := profile{time: a.Time, memory: a.MemoryKiB, lanes: a.Parallelism}
	if p.time == 0 {
		p.time = defaultTime
	}
	if p.memory == 0 {
		p.memory = defaultMemoryKiB
	}
	if p.lanes == 0 {
		p.lanes = defaultLanes
	}
	if p.time < minTime || p.time > maxTime {
		return profile{}, fmt.Errorf("password: profile t=%d outside [%d,%d]", p.time, minTime, maxTime)
	}
	if p.memory < minMemoryKiB || p.memory > maxMemoryKiB {
		return profile{}, fmt.Errorf("password: profile m=%d outside [%d,%d]", p.memory, minMemoryKiB, maxMemoryKiB)
	}
	if p.lanes < minLanes || p.lanes > maxLanes {
		return profile{}, fmt.Errorf("password: profile p=%d outside [%d,%d]", p.lanes, minLanes, maxLanes)
	}
	return p, nil
}

// Hash returns an Argon2id PHC string using the resolved profile.
func (a Argon2id) Hash(password string) (string, error) {
	p, err := a.resolve()
	if err != nil {
		return "", err
	}
	if len(password) > MaxPasswordLen {
		return "", fmt.Errorf("password: input exceeds %d bytes", MaxPasswordLen)
	}
	salt := make([]byte, saltLen)
	rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.lanes, keyLen)
	return format(p, salt, key), nil
}

// Compare verifies password using the parameters encoded in the hash.
func (a Argon2id) Compare(encoded, password string) error {
	if _, err := a.resolve(); err != nil {
		return err
	}
	if len(password) > MaxPasswordLen {
		return fmt.Errorf("password: input exceeds %d bytes", MaxPasswordLen)
	}
	stored, salt, key, err := decode(encoded)
	if err != nil {
		return err
	}
	// #nosec G115 -- decode bounds the key at maxKeyLen bytes
	got := argon2.IDKey([]byte(password), salt, stored.time, stored.memory, stored.lanes, uint32(len(key)))
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash reports whether encoded is invalid or differs from the resolved profile.
func (a Argon2id) NeedsRehash(encoded string) bool {
	p, err := a.resolve()
	if err != nil {
		return true
	}
	stored, salt, key, err := decode(encoded)
	if err != nil {
		return true
	}
	return stored != p || len(salt) != saltLen || len(key) != keyLen
}

// selfCheck ensures a hasher round-trips a password and reports mismatches as ErrMismatch.
func selfCheck(h Hasher) error {
	const probe = "self-check probe"
	encoded, err := h.Hash(probe)
	if err != nil {
		return fmt.Errorf("password: self-check hash: %w", err)
	}
	if err := h.Compare(encoded, probe); err != nil {
		return fmt.Errorf("password: self-check compare: %w", err)
	}
	if err := h.Compare(encoded, probe+"x"); !errors.Is(err, ErrMismatch) {
		return fmt.Errorf("password: self-check: wrong password returned %v, want ErrMismatch", err)
	}
	return nil
}

func format(p profile, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		phcVersion, p.memory, p.time, p.lanes, b64.EncodeToString(salt), b64.EncodeToString(key))
}

func decode(encoded string) (profile, []byte, []byte, error) {
	fail := func(format string, args ...any) (profile, []byte, []byte, error) {
		return profile{}, nil, nil, fmt.Errorf("%w: "+format, append([]any{ErrInvalidHash}, args...)...)
	}
	if len(encoded) > maxEncodedLen {
		return fail("%d bytes, over the %d the bounds allow", len(encoded), maxEncodedLen)
	}
	fields := strings.Split(encoded, "$")
	if len(fields) != 6 || fields[0] != "" {
		return fail("want 5 segments, got %d", len(fields)-1)
	}
	if fields[1] != "argon2id" {
		return fail("algorithm %q, want argon2id", fields[1])
	}
	v, err := numField(fields[2], "v")
	if err != nil {
		return fail("%v", err)
	}
	if v != phcVersion {
		return fail("version %d, want %d", v, phcVersion)
	}
	costs := strings.Split(fields[3], ",")
	if len(costs) != 3 {
		return fail("want 3 parameters, got %d", len(costs))
	}
	memory, err := numField(costs[0], "m")
	if err != nil {
		return fail("%v", err)
	}
	time, err := numField(costs[1], "t")
	if err != nil {
		return fail("%v", err)
	}
	lanes, err := numField(costs[2], "p")
	if err != nil {
		return fail("%v", err)
	}
	if memory < minMemoryKiB || memory > maxMemoryKiB {
		return fail("m=%d outside [%d,%d]", memory, minMemoryKiB, maxMemoryKiB)
	}
	if time < minTime || time > maxTime {
		return fail("t=%d outside [%d,%d]", time, minTime, maxTime)
	}
	if lanes < minLanes || lanes > maxLanes {
		return fail("p=%d outside [%d,%d]", lanes, minLanes, maxLanes)
	}
	salt, err := decodeBounded(fields[4], "salt", minSaltLen, maxSaltLen)
	if err != nil {
		return fail("%v", err)
	}
	key, err := decodeBounded(fields[5], "hash", minKeyLen, maxKeyLen)
	if err != nil {
		return fail("%v", err)
	}
	// #nosec G115 -- lanes is bounded at maxLanes above
	return profile{time: time, memory: memory, lanes: uint8(lanes)}, salt, key, nil
}

func numField(s, name string) (uint32, error) {
	prefix := name + "="
	if !strings.HasPrefix(s, prefix) {
		return 0, fmt.Errorf("%q is not %s=<number>", s, name)
	}
	n, err := strconv.ParseUint(s[len(prefix):], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s: %v", name, err)
	}
	return uint32(n), nil
}

// decodeBounded rejects oversized input before allocating decoded storage.
func decodeBounded(s, name string, minLen, maxLen int) ([]byte, error) {
	if n := b64.DecodedLen(len(s)); n < minLen || n > maxLen {
		return nil, fmt.Errorf("%s length outside [%d,%d]", name, minLen, maxLen)
	}
	out, err := b64.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	if len(out) < minLen || len(out) > maxLen {
		return nil, fmt.Errorf("%s is %d bytes, want [%d,%d]", name, len(out), minLen, maxLen)
	}
	return out, nil
}
