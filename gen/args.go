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

	inOptions := false
	for _, tok := range lexArgs(a.Args) {
		key, val, isOption := cutOption(tok)
		switch {
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
			out.Pos = append(out.Pos, tok)
		}
	}

	if len(out.Pos) < len(m.Pos) {
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

// cutOption treats key=value as an option only when key is an identifier.
func cutOption(tok Arg) (key string, val Arg, ok bool) {
	i := strings.IndexByte(tok.Text, '=')
	if i <= 0 || !isIdent(tok.Text[:i]) {
		return "", Arg{}, false
	}
	return tok.Text[:i], Arg{Text: tok.Text[i+1:], Col: tok.Col + i + 1}, true
}

func isIdent(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' {
			continue
		}
		return false
	}
	return s != ""
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

// lexArgs splits whitespace-separated args, preserving quoted spans.
func lexArgs(s string) []Arg {
	var out []Arg
	var cur strings.Builder
	start := -1
	inQuote := false
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b == '"':
			inQuote = !inQuote
			if start < 0 {
				start = i
			}
		case (b == ' ' || b == '\t') && !inQuote:
			if start >= 0 {
				out = append(out, Arg{Text: cur.String(), Col: start})
				cur.Reset()
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
			cur.WriteByte(b)
		}
	}
	if start >= 0 {
		out = append(out, Arg{Text: cur.String(), Col: start})
	}
	return out
}
