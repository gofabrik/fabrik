package assetmapper

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"
)

// HashLength is the number of SHA-256 hex characters embedded in compiled filenames.
const HashLength = 8

// hashContent returns the truncated digest used in public filenames.
func hashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:HashLength]
}

// hashedName appends the hash before the file extension.
func hashedName(logicalPath, hash string) string {
	dir, base := path.Split(logicalPath)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return dir + stem + "-" + hash + ext
}
