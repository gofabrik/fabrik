package storage

import (
	"crypto/rand"
	"encoding/hex"
)

// UniqueKey derives a collision-resistant key under dir: a random
// 128-bit hex prefix keeps name only when every rune is an RFC 3986
// unreserved character, so the key embeds in URL paths untouched; an
// unusable name leaves the bare prefix. dir is used as given, so the
// result satisfies [CheckKey] exactly when dir does. crypto/rand
// never returns an error (it aborts on entropy failure).
func UniqueKey(dir, name string) string {
	var buf [16]byte
	rand.Read(buf[:]) //nolint:errcheck // never fails by contract
	key := hex.EncodeToString(buf[:])
	if urlPathSafe(name) {
		key += "-" + name
	}
	if dir != "" {
		key = dir + "/" + key
	}
	return key
}

// urlPathSafe reports whether s consists of RFC 3986 unreserved
// characters only.
func urlPathSafe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '.', r == '_', r == '~':
		default:
			return false
		}
	}
	return true
}
