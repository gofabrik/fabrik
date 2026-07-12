package gen

import "unicode"

// LowerFirst lowercases the leading rune, for deriving generated
// identifiers from exported Go names.
func LowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}
