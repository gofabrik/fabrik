// Package sessionutil provides helpers for session stores.
package sessionutil

// ClonePayload copies payload bytes across the store boundary.
func ClonePayload(p []byte) []byte {
	if p == nil {
		return nil
	}
	out := make([]byte, len(p))
	copy(out, p)
	return out
}
