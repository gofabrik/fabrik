package gen

import (
	"fmt"
	"strings"

	"github.com/gofabrik/fabrik/diag"
)

// Arg is one token from directive arguments.
type Arg struct {
	Text string
	Col  int // byte offset within Annotation.Args
}

// lexToken is an Arg plus its source mapping, private to the lexer so
// Arg stays comparable.
type lexToken struct {
	Text string
	Col  int
	// srcCols[i] is the offset within Annotation.Args of the byte that
	// produced Text[i]; quotes and escapes make the mapping non-contiguous.
	srcCols []int
}

// Args contains positional arguments and key=value options.
type Args struct {
	Pos  []Arg
	Attr map[string]Arg
}

// ParseArgs applies Meta's argument shape without validating value semantics.
func ParseArgs(a Annotation, m Meta) (Args, diag.Diagnostics) {
	var ds diag.Diagnostics
	out := Args{Attr: map[string]Arg{}}
	directive := "//fabrik:" + a.Name

	known := map[string]AttrSpec{}
	for _, s := range m.Attrs {
		known[s.Key] = s
	}

	toks, openQuote := lexArgs(a.Args)
	if openQuote >= 0 {
		ds.Error(a.ArgPos(openQuote), "unterminated quote",
			`close the quoted value, or write \" for a quote inside it`)
	}

	inOptions := false
	for _, tok := range toks {
		key, val, isOption := cutOption(tok)
		switch {
		case isOption && !isIdent(key):
			inOptions = true
			ds.Error(a.ArgPos(tok.Col), fmt.Sprintf("invalid option key %q for %s", key, directive), knownOptionsHelp(m))
		case isOption:
			inOptions = true
			spec, ok := known[key]
			if !ok {
				ds.Error(a.ArgPos(tok.Col), fmt.Sprintf("unknown option %q for %s", key, directive), knownOptionsHelp(m))
				continue
			}
			if _, dup := out.Attr[spec.Key]; dup {
				ds.Error(a.ArgPos(tok.Col), fmt.Sprintf("duplicate option %q", key), "")
				continue
			}
			out.Attr[spec.Key] = val
		case inOptions:
			ds.Error(a.ArgPos(tok.Col), fmt.Sprintf("positional argument %q after options", tok.Text),
				"options (key=value) must come last")
		case len(out.Pos) >= len(m.Pos):
			if len(m.Pos) == 0 {
				ds.Error(a.ArgPos(tok.Col), fmt.Sprintf("%s takes no arguments (got %q)", directive, tok.Text),
					exampleHelp(m))
			} else {
				ds.Error(a.ArgPos(tok.Col), fmt.Sprintf("unexpected argument %q", tok.Text),
					fmt.Sprintf("%s takes only %s", directive, posNames(m)))
			}
		default:
			out.Pos = append(out.Pos, Arg{Text: tok.Text, Col: tok.Col})
		}
	}

	if len(out.Pos) < requiredPos(m) {
		missing := m.Pos[len(out.Pos)].Name
		msg := fmt.Sprintf("%s requires %s", directive, posNames(m))
		if len(out.Pos) > 0 {
			msg = fmt.Sprintf("%s requires %s after %s (got only %q)",
				directive, missing, m.Pos[len(out.Pos)-1].Name, out.Pos[len(out.Pos)-1].Text)
		}
		ds.Error(a.Pos, msg, exampleHelp(m))
	}
	for _, s := range m.Attrs {
		if _, ok := out.Attr[s.Key]; s.Required && !ok {
			ds.Error(a.Pos, fmt.Sprintf("%s requires %s=", directive, s.Key), exampleHelp(m))
		}
	}
	return out, ds
}

// cutOption accepts key=value only for identifier keys and reports the value's source offset.
func cutOption(tok lexToken) (key string, val Arg, ok bool) {
	i := strings.IndexByte(tok.Text, '=')
	if i <= 0 || !isIdentChars(tok.Text[:i]) {
		return "", Arg{}, false
	}
	col := tok.srcCols[i] + 1
	if i+1 < len(tok.Text) {
		col = tok.srcCols[i+1]
	}
	return tok.Text[:i], Arg{Text: tok.Text[i+1:], Col: col}, true
}

func isIdent(s string) bool {
	return isIdentChars(s) && (s[0] < '0' || s[0] > '9')
}

func isIdentChars(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' {
			continue
		}
		return false
	}
	return s != ""
}

// requiredPos counts the leading non-optional positionals.
func requiredPos(m Meta) int {
	n := 0
	for _, p := range m.Pos {
		if p.Optional {
			break
		}
		n++
	}
	return n
}

func posNames(m Meta) string {
	names := make([]string, len(m.Pos))
	for i, p := range m.Pos {
		names[i] = p.Name
	}
	return strings.Join(names, " and ")
}

func knownOptionsHelp(m Meta) string {
	if len(m.Attrs) == 0 {
		return "this directive takes no options"
	}
	keys := make([]string, len(m.Attrs))
	for i, s := range m.Attrs {
		keys[i] = s.Key + "="
	}
	return "known options: " + strings.Join(keys, ", ")
}

func exampleHelp(m Meta) string {
	if m.Example == "" {
		return ""
	}
	return "example: " + m.Example
}

// lexArgs splits on unquoted whitespace, removes quotes, and recognizes \" and \\ in quoted
// spans. It preserves source offsets and returns an unmatched quote offset with the final token.
func lexArgs(s string) ([]lexToken, int) {
	var out []lexToken
	var cur []byte
	var cols []int
	start := -1
	openQuote := -1
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b == '\\' && openQuote >= 0 && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\'):
			i++
			cur = append(cur, s[i])
			cols = append(cols, i)
		case b == '"':
			if start < 0 {
				start = i
			}
			if openQuote < 0 {
				openQuote = i
			} else {
				openQuote = -1
			}
		case (b == ' ' || b == '\t') && openQuote < 0:
			if start >= 0 {
				out = append(out, lexToken{Text: string(cur), Col: start, srcCols: cols})
				cur, cols, start = nil, nil, -1
			}
		default:
			if start < 0 {
				start = i
			}
			cur = append(cur, b)
			cols = append(cols, i)
		}
	}
	if start >= 0 {
		out = append(out, lexToken{Text: string(cur), Col: start, srcCols: cols})
	}
	return out, openQuote
}
