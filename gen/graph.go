package gen

import (
	"go/scanner"
	"go/token"
	"strings"
)

// defines returns variables n introduces in the surrounding scope.
func defines(n Node) []string {
	switch n := n.(type) {
	case *Raw:
		return n.Defines
	case *Assign:
		return []string{n.Var}
	case *Call:
		if n.Var != "" && n.Err != ErrInline {
			if n.Cleanup != "" {
				return []string{n.Var, n.Cleanup}
			}
			return []string{n.Var}
		}
	case *ConfigLoad:
		return []string{n.Var}
	case *StructLit:
		return []string{n.Var}
	case *Select:
		return []string{n.Var}
	}
	return nil
}

// uses returns names from vars that n references.
func uses(n Node, vars map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if vars[name] && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, u := range n.base().Uses {
		add(u)
	}
	own := map[string]bool{}
	for _, d := range defines(n) {
		own[d] = true
	}

	src := []byte(strings.Join(renderNode(n), "\n"))
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var sc scanner.Scanner
	sc.Init(file, src, nil, 0)
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.IDENT && !own[lit] {
			add(lit)
		}
	}
	return out
}
