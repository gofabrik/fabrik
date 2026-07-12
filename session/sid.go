package session

import (
	"crypto/rand"
	"encoding/base64"
)

// sidByteLen sizes the random SID: 32 bytes of entropy, 43 characters
// base64url-encoded.
const sidByteLen = 32

// generateSID is the default session-ID generator.
func generateSID() (string, error) {
	buf := make([]byte, sidByteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
