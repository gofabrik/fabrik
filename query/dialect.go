package query

import (
	"fmt"
	"strconv"
	"strings"
)

// Dialect selects the placeholder style for generated write SQL.
type Dialect int

const (
	DialectSQLite Dialect = iota
	DialectPostgres
)

func (d Dialect) String() string {
	switch d {
	case DialectSQLite:
		return "sqlite"
	case DialectPostgres:
		return "postgres"
	}
	return fmt.Sprintf("Dialect(%d)", int(d))
}

// checkDialect rejects dialect values outside the enum.
func checkDialect(fn string, d Dialect) error {
	switch d {
	case DialectSQLite, DialectPostgres:
		return nil
	}
	return fmt.Errorf("%s: unknown dialect %s", fn, d)
}

// finalize applies the dialect's placeholder style.
func finalize(fn string, d Dialect, stmt string) (string, error) {
	if err := checkDialect(fn, d); err != nil {
		return "", err
	}
	if d == DialectPostgres {
		return rebindPostgres(stmt), nil
	}
	return stmt, nil
}

// rebindPostgres rewrites every unquoted ? to $1, $2, and so on.
// Comments and dollar-quoted strings are rejected by checkWhere.
func rebindPostgres(stmt string) string {
	var b strings.Builder
	b.Grow(len(stmt) + 8)
	n := 0
	for i := 0; i < len(stmt); i++ {
		switch c := stmt[i]; c {
		case '\'', '"':
			j, _ := skipQuoted(stmt, i)
			b.WriteString(stmt[i:j])
			i = j - 1
		case '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// skipQuoted scans SQL standard quoted strings and identifiers.
func skipQuoted(stmt string, i int) (end int, ok bool) {
	quote := stmt[i]
	for j := i + 1; j < len(stmt); j++ {
		if stmt[j] != quote {
			continue
		}
		if j+1 < len(stmt) && stmt[j+1] == quote {
			j++
			continue
		}
		return j + 1, true
	}
	return len(stmt), false
}

// checkWhere rejects fragments the rebinder cannot parse safely.
// Write-helper placeholders are always bare ?. Use raw SQL for
// comments, dollar-quoted strings, escape strings, or JSONB ?
// operators.
func checkWhere(fn, where string) error {
	if strings.TrimSpace(where) == "" {
		return fmt.Errorf("%s: empty where - to affect every row deliberately, use a constant true predicate like \"1 = 1\"", fn)
	}
	for i := 0; i < len(where); i++ {
		switch c := where[i]; c {
		case '\'', '"':
			if c == '\'' && isEscapeStringPrefix(where, i) {
				return fmt.Errorf("%s: where contains a Postgres E'' escape-string literal - the rebinder does not parse backslash escapes; use standard '' quoting or raw SQL", fn)
			}
			end, ok := skipQuoted(where, i)
			if !ok {
				return fmt.Errorf("%s: where contains an unterminated %c quote", fn, c)
			}
			i = end - 1
		case '?':
			if i+1 < len(where) {
				next := where[i+1]
				if next == '&' || (next == '|' && (i+2 >= len(where) || where[i+2] != '|')) {
					return fmt.Errorf("%s: where contains the JSONB ?%c operator - write-helper where treats every unquoted ? as a placeholder; use raw SQL for JSONB operators", fn, next)
				}
				// Numbered placeholders conflict with the helper's
				// left-to-right binding model.
				if next >= '0' && next <= '9' {
					j := i + 1
					for j < len(where) && where[j] >= '0' && where[j] <= '9' {
						j++
					}
					return fmt.Errorf("%s: where contains the numbered placeholder %q - write-helper placeholders are always bare ?, ordered left to right", fn, where[i:j])
				}
			}
			for j := i + 1; j < len(where); j++ {
				if isSQLSpace(where[j]) {
					continue
				}
				if where[j] == '\'' {
					return fmt.Errorf("%s: where contains ? followed by a string literal (JSONB ?-operator pattern) - write-helper where treats every unquoted ? as a placeholder; use raw SQL for JSONB operators", fn)
				}
				if where[j] == '?' {
					return fmt.Errorf("%s: where contains ? followed by another ? (JSONB ?-operator with a bound value) - write-helper where treats every unquoted ? as a placeholder; use raw SQL for JSONB operators", fn)
				}
				if where[j] == '(' {
					return fmt.Errorf("%s: where contains ? followed by a parenthesized expression (JSONB ?-operator pattern) - write-helper where treats every unquoted ? as a placeholder; use raw SQL for JSONB operators", fn)
				}
				break
			}
		case '$':
			if i+1 < len(where) && where[i+1] >= '0' && where[i+1] <= '9' {
				return fmt.Errorf("%s: where contains %q - write-helper placeholders are always ?, the library numbers them for the dialect", fn, dollarToken(where, i))
			}
			if end := dollarTagEnd(where, i); end > 0 {
				return fmt.Errorf("%s: where contains the dollar-quote delimiter %q - the rebinder does not parse dollar-quoted strings; use raw SQL for this query", fn, where[i:end])
			}
		case '-':
			if i+1 < len(where) && where[i+1] == '-' {
				return fmt.Errorf("%s: where contains a -- comment - the rebinder does not parse comments; use raw SQL for this query", fn)
			}
		case '/':
			if i+1 < len(where) && where[i+1] == '*' {
				return fmt.Errorf("%s: where contains a /* comment - the rebinder does not parse comments; use raw SQL for this query", fn)
			}
		}
	}
	return nil
}

// isEscapeStringPrefix reports whether the quote at i opens an
// E-string literal.
func isEscapeStringPrefix(where string, i int) bool {
	if i == 0 {
		return false
	}
	if c := where[i-1]; c != 'E' && c != 'e' {
		return false
	}
	if i == 1 {
		return true
	}
	return !isIdentChar(where[i-2])
}

func isIdentChar(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// isSQLSpace covers whitespace accepted between SQL tokens.
func isSQLSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}

// dollarTagEnd reports a dollar-quote delimiter opening at i.
func dollarTagEnd(where string, i int) int {
	if i+1 < len(where) && where[i+1] == '$' {
		return i + 2
	}
	j := i + 1
	if j >= len(where) || !isTagStart(where[j]) {
		return 0
	}
	for j++; j < len(where); j++ {
		c := where[j]
		if c == '$' {
			return j + 1
		}
		if !isTagStart(c) && (c < '0' || c > '9') {
			return 0
		}
	}
	return 0
}

func isTagStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// dollarToken returns the $<digits> token starting at i.
func dollarToken(where string, i int) string {
	j := i + 1
	for j < len(where) && where[j] >= '0' && where[j] <= '9' {
		j++
	}
	return where[i:j]
}
