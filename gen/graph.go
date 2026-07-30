package gen

import (
	"fmt"
	"go/ast"
	"go/parser"
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

func exprFields(n Node) []string {
	switch n := n.(type) {
	case *Assign:
		return []string{n.Expr}
	case *Call:
		return append([]string{n.Fn}, n.Args...)
	case *ConfigLoad:
		fields := n.Options
		if n.Prefix != "" {
			fields = append([]string{n.Prefix}, fields...)
		}
		return fields
	case *StructLit:
		fields := make([]string, 0, len(n.Fields))
		for _, f := range n.Fields {
			fields = append(fields, f.Expr)
		}
		return fields
	case *Select:
		return []string{n.KeyExpr}
	case *Route:
		return append([]string{n.Router, n.Handler}, n.Chain...)
	}
	return nil
}

// uses returns explicit and inferred dependencies, including Select children.
func uses(n Node, vars map[string]bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if vars[name] && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	own := map[string]bool{}
	var markOwn func(m Node)
	markOwn = func(m Node) {
		for _, d := range defines(m) {
			own[d] = true
		}
		if sel, ok := m.(*Select); ok {
			for _, c := range sel.Cases {
				for _, child := range c.Body {
					markOwn(child)
				}
				markOwn(&c.Result)
			}
		}
	}
	markOwn(n)
	addFree := func(name string) {
		if !own[name] {
			add(name)
		}
	}

	for _, u := range n.base().Uses {
		add(u)
	}
	var collect func(m Node)
	collect = func(m Node) {
		for _, src := range exprFields(m) {
			freeIdents(src, addFree)
		}
		for _, u := range m.base().Uses {
			addFree(u)
		}
		if sel, ok := m.(*Select); ok {
			for _, c := range sel.Cases {
				for _, child := range c.Body {
					collect(child)
				}
				collect(&c.Result)
			}
		}
	}
	collect(n)
	return out
}

// freeIdents excludes named struct keys and function-local names; validation handles parse errors.
func freeIdents(src string, add func(string)) {
	e, err := parser.ParseExpr(src)
	if err != nil {
		return
	}
	walkExpr(e, &scopeStack{}, add)
}

// validateExprFields attributes unparsable expressions to the emitting directive.
func validateExprFields(nodes []Node) error {
	for _, n := range nodes {
		var check func(m Node, nested bool) error
		check = func(m Node, nested bool) error {
			for _, src := range exprFields(m) {
				if _, err := parser.ParseExpr(src); err != nil {
					return fmt.Errorf("directive %q emitted an unparsable expression %q: %v", n.base().Origin.Directive, src, err)
				}
			}
			if raw, ok := m.(*Raw); ok && rawReturns(raw.Lines) {
				return fmt.Errorf("directive %q emitted a return statement in raw lines; assign err and set Check instead", n.base().Origin.Directive)
			}
			if c, ok := m.(*Call); ok && nested && c.Cleanup != "" {
				return fmt.Errorf("directive %q emitted a cleanup-bearing call inside a select case; cleanup calls must be top-level", n.base().Origin.Directive)
			}
			if sel, ok := m.(*Select); ok {
				for _, c := range sel.Cases {
					for _, child := range c.Body {
						if err := check(child, true); err != nil {
							return err
						}
					}
					if err := check(&c.Result, true); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := check(n, false); err != nil {
			return err
		}
	}
	return nil
}

// rawReturns detects returns outside function literals and leaves parse errors to findUnparsable.
func rawReturns(lines []string) bool {
	src := "package p\nfunc _() {\n" + strings.Join(lines, "\n") + "\n}"
	f, err := parser.ParseFile(token.NewFileSet(), "", src, 0)
	if err != nil {
		return false
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			found = true
			return false
		}
		return true
	})
	return found
}

type scopeStack struct {
	frames []map[string]bool
}

func (s *scopeStack) push() { s.frames = append(s.frames, map[string]bool{}) }
func (s *scopeStack) pop()  { s.frames = s.frames[:len(s.frames)-1] }

func (s *scopeStack) bind(name string) {
	if name == "" || name == "_" || len(s.frames) == 0 {
		return
	}
	s.frames[len(s.frames)-1][name] = true
}

func (s *scopeStack) bound(name string) bool {
	for i := len(s.frames) - 1; i >= 0; i-- {
		if s.frames[i][name] {
			return true
		}
	}
	return false
}

func walkExpr(n ast.Expr, sc *scopeStack, add func(string)) {
	switch n := n.(type) {
	case nil:
	case *ast.Ident:
		if !sc.bound(n.Name) {
			add(n.Name)
		}
	case *ast.SelectorExpr:
		walkExpr(n.X, sc, add)
	case *ast.CompositeLit:
		walkExpr(n.Type, sc, add)
		_, isMap := n.Type.(*ast.MapType)
		_, isArray := n.Type.(*ast.ArrayType)
		exprKeys := isMap || isArray
		for _, elt := range n.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				walkExpr(elt, sc, add)
				continue
			}
			if _, ident := kv.Key.(*ast.Ident); !ident || exprKeys {
				walkExpr(kv.Key, sc, add)
			}
			walkExpr(kv.Value, sc, add)
		}
	case *ast.FuncLit:
		// Parameter and result types use the outer scope, while declared names bind only in the body.
		walkFieldTypes(n.Type.Params, sc, add)
		walkFieldTypes(n.Type.Results, sc, add)
		sc.push()
		bindFieldList(n.Type.Params, sc)
		bindFieldList(n.Type.Results, sc)
		walkStmts(n.Body.List, sc, add)
		sc.pop()
	case *ast.CallExpr:
		walkExpr(n.Fun, sc, add)
		for _, a := range n.Args {
			walkExpr(a, sc, add)
		}
	case *ast.IndexExpr:
		walkExpr(n.X, sc, add)
		walkExpr(n.Index, sc, add)
	case *ast.IndexListExpr:
		walkExpr(n.X, sc, add)
		for _, i := range n.Indices {
			walkExpr(i, sc, add)
		}
	case *ast.StarExpr:
		walkExpr(n.X, sc, add)
	case *ast.UnaryExpr:
		walkExpr(n.X, sc, add)
	case *ast.BinaryExpr:
		walkExpr(n.X, sc, add)
		walkExpr(n.Y, sc, add)
	case *ast.ParenExpr:
		walkExpr(n.X, sc, add)
	case *ast.TypeAssertExpr:
		walkExpr(n.X, sc, add)
		walkExpr(n.Type, sc, add)
	case *ast.SliceExpr:
		walkExpr(n.X, sc, add)
		walkExpr(n.Low, sc, add)
		walkExpr(n.High, sc, add)
		walkExpr(n.Max, sc, add)
	case *ast.KeyValueExpr:
		walkExpr(n.Key, sc, add)
		walkExpr(n.Value, sc, add)
	case *ast.ArrayType:
		walkExpr(n.Len, sc, add)
		walkExpr(n.Elt, sc, add)
	case *ast.MapType:
		walkExpr(n.Key, sc, add)
		walkExpr(n.Value, sc, add)
	case *ast.ChanType:
		walkExpr(n.Value, sc, add)
	case *ast.FuncType:
		walkFieldTypes(n.Params, sc, add)
		walkFieldTypes(n.Results, sc, add)
	case *ast.StructType:
		walkFieldTypes(n.Fields, sc, add)
	case *ast.InterfaceType:
		walkFieldTypes(n.Methods, sc, add)
	case *ast.Ellipsis:
		walkExpr(n.Elt, sc, add)
	}
}

func bindFieldList(fl *ast.FieldList, sc *scopeStack) {
	if fl == nil {
		return
	}
	for _, f := range fl.List {
		for _, name := range f.Names {
			sc.bind(name.Name)
		}
	}
}

func walkFieldTypes(fl *ast.FieldList, sc *scopeStack, add func(string)) {
	if fl == nil {
		return
	}
	for _, f := range fl.List {
		walkExpr(f.Type, sc, add)
	}
}

func walkStmts(list []ast.Stmt, sc *scopeStack, add func(string)) {
	for _, s := range list {
		walkStmt(s, sc, add)
	}
}

func walkStmt(s ast.Stmt, sc *scopeStack, add func(string)) {
	switch s := s.(type) {
	case nil:
	case *ast.BlockStmt:
		sc.push()
		walkStmts(s.List, sc, add)
		sc.pop()
	case *ast.AssignStmt:
		for _, r := range s.Rhs {
			walkExpr(r, sc, add)
		}
		for _, l := range s.Lhs {
			if id, ok := l.(*ast.Ident); ok && s.Tok.String() == ":=" {
				sc.bind(id.Name)
			} else {
				walkExpr(l, sc, add)
			}
		}
	case *ast.DeclStmt:
		gd, ok := s.Decl.(*ast.GenDecl)
		if !ok {
			return
		}
		for _, spec := range gd.Specs {
			switch spec := spec.(type) {
			case *ast.ValueSpec:
				walkExpr(spec.Type, sc, add)
				for _, v := range spec.Values {
					walkExpr(v, sc, add)
				}
				for _, name := range spec.Names {
					sc.bind(name.Name)
				}
			case *ast.TypeSpec:
				sc.bind(spec.Name.Name)
				walkExpr(spec.Type, sc, add)
			}
		}
	case *ast.ExprStmt:
		walkExpr(s.X, sc, add)
	case *ast.ReturnStmt:
		for _, r := range s.Results {
			walkExpr(r, sc, add)
		}
	case *ast.IfStmt:
		sc.push()
		walkStmt(s.Init, sc, add)
		walkExpr(s.Cond, sc, add)
		walkStmt(s.Body, sc, add)
		walkStmt(s.Else, sc, add)
		sc.pop()
	case *ast.ForStmt:
		sc.push()
		walkStmt(s.Init, sc, add)
		walkExpr(s.Cond, sc, add)
		walkStmt(s.Post, sc, add)
		walkStmt(s.Body, sc, add)
		sc.pop()
	case *ast.RangeStmt:
		sc.push()
		walkExpr(s.X, sc, add)
		for _, e := range []ast.Expr{s.Key, s.Value} {
			if id, ok := e.(*ast.Ident); ok && s.Tok.String() == ":=" {
				sc.bind(id.Name)
			} else {
				walkExpr(e, sc, add)
			}
		}
		walkStmt(s.Body, sc, add)
		sc.pop()
	case *ast.SwitchStmt:
		sc.push()
		walkStmt(s.Init, sc, add)
		walkExpr(s.Tag, sc, add)
		walkStmt(s.Body, sc, add)
		sc.pop()
	case *ast.TypeSwitchStmt:
		// Case types use the outer scope; the guard binds only in each clause body.
		sc.push()
		walkStmt(s.Init, sc, add)
		guard := ""
		switch a := s.Assign.(type) {
		case *ast.AssignStmt:
			if id, ok := a.Lhs[0].(*ast.Ident); ok {
				guard = id.Name
			}
			for _, r := range a.Rhs {
				walkExpr(r, sc, add)
			}
		case *ast.ExprStmt:
			walkExpr(a.X, sc, add)
		}
		for _, cs := range s.Body.List {
			cc, ok := cs.(*ast.CaseClause)
			if !ok {
				continue
			}
			sc.push()
			for _, e := range cc.List {
				walkExpr(e, sc, add)
			}
			sc.bind(guard)
			walkStmts(cc.Body, sc, add)
			sc.pop()
		}
		sc.pop()
	case *ast.SelectStmt:
		walkStmt(s.Body, sc, add)
	case *ast.CaseClause:
		sc.push()
		for _, e := range s.List {
			walkExpr(e, sc, add)
		}
		walkStmts(s.Body, sc, add)
		sc.pop()
	case *ast.CommClause:
		sc.push()
		walkStmt(s.Comm, sc, add)
		walkStmts(s.Body, sc, add)
		sc.pop()
	case *ast.SendStmt:
		walkExpr(s.Chan, sc, add)
		walkExpr(s.Value, sc, add)
	case *ast.IncDecStmt:
		walkExpr(s.X, sc, add)
	case *ast.GoStmt:
		walkExpr(s.Call, sc, add)
	case *ast.DeferStmt:
		walkExpr(s.Call, sc, add)
	case *ast.LabeledStmt:
		walkStmt(s.Stmt, sc, add)
	case *ast.BranchStmt:
	case *ast.EmptyStmt:
	}
}
