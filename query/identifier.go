package query

import (
	"fmt"
	"regexp"
	"strings"
)

// identifierRe matches one or more dot-separated SQL identifiers,
// each either
//
//   - a bare identifier: [A-Za-z_][A-Za-z0-9_$]*, or
//   - a double-quoted identifier: "..." with "" escaping an embedded
//     quote (the SQL-standard quoting that SQLite and PostgreSQL
//     use), at least one character long - PostgreSQL rejects
//     zero-length quoted identifiers, so the validator does too.
//
// Whitespace, statement separators, comments, and stray quotes are rejected.
var identifierRe = regexp.MustCompile(
	`^(?:[A-Za-z_][A-Za-z0-9_$]*|"(?:[^"]|"")+")` +
		`(?:\.(?:[A-Za-z_][A-Za-z0-9_$]*|"(?:[^"]|"")+"))*$`)

// validIdentifier reports whether s is safe to interpolate as an
// optional schema-qualified identifier.
func validIdentifier(s string) bool {
	return identifierRe.MatchString(s)
}

// columnRe is one unqualified identifier segment.
var columnRe = regexp.MustCompile(
	`^(?:[A-Za-z_][A-Za-z0-9_$]*|"(?:[^"]|"")+")$`)

// validColumn reports whether s can serve as a generated column name.
func validColumn(s string) bool {
	return columnRe.MatchString(s)
}

// unquoteIdent returns the logical column name for scan matching.
//
// Drivers report quoted result columns without surrounding quotes.
func unquoteIdent(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	return s
}

// checkTable validates a write helper's table argument.
func checkTable(fn, table string) error {
	if !validIdentifier(table) {
		return fmt.Errorf(
			"%w: %s: table %q — table names are interpolated, not "+
				"parameterized, so they must be developer-controlled "+
				"constants, never user input",
			ErrInvalidIdentifier, fn, table)
	}
	return nil
}
