// Package load reads typed directive annotations from a fabrik project.
package load

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
	"golang.org/x/tools/go/packages"
)

// Item is one directive annotation with its semantic view.
type Item struct {
	Ann   gen.Annotation
	Typed gen.Typed
}

// Result is the loaded view of a project.
type Result struct {
	ModulePath string
	Root       string
	MainDir    string // directory of package main, where main.gen.go is written
	Items      []Item
	Diags      diag.Diagnostics // typo warnings, ignored-in-main warnings, type errors
	// Types maps an import path to its type-checked package, including the
	// transitive imports of the app (so a directive can look up a library
	// type like jobs.Store by path even when no annotated signature names
	// it).
	Types map[string]*types.Package
}

// Load type-checks the module rooted at dir and collects //fabrik: annotations.
func Load(dir string, overlay map[string][]byte) (*Result, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedModule,
		Dir:     dir,
		Overlay: overlay,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no Go packages found under %s", dir)
	}

	res := &Result{Root: dir}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].PkgPath < pkgs[j].PkgPath })

	var mains []*packages.Package
	for _, pkg := range pkgs {
		if res.ModulePath == "" && pkg.Module != nil {
			res.ModulePath = pkg.Module.Path
			res.Root = pkg.Module.Dir
		}
		if pkg.Name == "main" {
			mains = append(mains, pkg)
			// main.gen.go may be stale; handwritten parse errors still block wiring.
			var parseErrs []packages.Error
			for _, e := range pkg.Errors {
				if e.Kind == packages.ParseError && filepath.Base(errorPosition(e).Filename) != "main.gen.go" {
					parseErrs = append(parseErrs, e)
				}
			}
			reportPkgErrors(parseErrs, &res.Diags)
			warnMainDirectives(pkg, &res.Diags)
			continue
		}
		reportPkgErrors(pkg.Errors, &res.Diags)
		scanPackage(pkg, res)
	}

	mainDir, err := selectMain(mains)
	if err != nil {
		return nil, err
	}
	res.MainDir = mainDir

	res.Types = map[string]*types.Package{}
	for _, pkg := range pkgs {
		collectTypes(pkg.Types, res.Types)
	}

	res.Diags.Sort()
	sort.SliceStable(res.Items, func(i, j int) bool {
		a, b := res.Items[i].Ann.Pos, res.Items[j].Ann.Pos
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		return a.Line < b.Line
	})
	return res, nil
}

// collectTypes records p and its transitive imports by path.
func collectTypes(p *types.Package, into map[string]*types.Package) {
	if p == nil || into[p.Path()] != nil {
		return
	}
	into[p.Path()] = p
	for _, imp := range p.Imports() {
		collectTypes(imp, into)
	}
}

func scanPackage(pkg *packages.Package, res *Result) {
	for _, file := range pkg.Syntax {
		anns, ds := ScanFile(pkg.Fset, file)
		res.Diags = append(res.Diags, ds...)
		for _, ann := range anns {
			var target types.Object
			switch n := ann.Decl.(type) {
			case *ast.FuncDecl:
				target = pkg.TypesInfo.Defs[n.Name]
			case *ast.TypeSpec:
				target = pkg.TypesInfo.Defs[n.Name]
			case *ast.ValueSpec:
				if len(n.Names) == 1 {
					target = pkg.TypesInfo.Defs[n.Names[0]]
				}
			}
			res.Items = append(res.Items, Item{
				Ann: ann,
				Typed: gen.Typed{
					Target: target,
					Fset:   pkg.Fset,
				},
			})
		}
	}
}

// ScanFile extracts //fabrik: annotations from a parsed file.
func ScanFile(fset *token.FileSet, file *ast.File) ([]gen.Annotation, diag.Diagnostics) {
	var anns []gen.Annotation
	var ds diag.Diagnostics
	consumed := map[*ast.CommentGroup]bool{}
	scan := func(doc *ast.CommentGroup, target ast.Node) {
		consumed[doc] = true
		docLines := make([]string, 0, len(doc.List))
		for _, c := range doc.List {
			docLines = append(docLines, c.Text)
		}
		for _, c := range doc.List {
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			if !strings.HasPrefix(text, "fabrik:") {
				maybeTypoWarning(fset.Position(c.Slash), text, &ds)
				continue
			}
			name, args := splitFirst(strings.TrimPrefix(text, "fabrik:"))
			pos := fset.Position(c.Slash)
			anns = append(anns, gen.Annotation{
				Name:    name,
				Args:    args,
				Pos:     pos,
				ArgsCol: pos.Column + argsOffset(c.Text, name, args),
				Decl:    target,
				Doc:     docLines,
			})
		}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Doc != nil {
				scan(d.Doc, d)
			}
		case *ast.GenDecl:
			if d.Tok != token.TYPE && d.Tok != token.VAR {
				continue
			}
			// Match standard Go doc attribution for single-spec declarations.
			if d.Doc != nil && len(d.Specs) == 1 {
				scan(d.Doc, d.Specs[0])
			}
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					if sp.Doc != nil {
						scan(sp.Doc, sp)
					}
				case *ast.ValueSpec:
					if sp.Doc != nil {
						scan(sp.Doc, sp)
					}
				}
			}
		}
	}
	// Detached directives are ignored by Go doc attribution, so report them.
	for _, cg := range file.Comments {
		if consumed[cg] {
			continue
		}
		for _, c := range cg.List {
			text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			if !strings.HasPrefix(text, "fabrik:") {
				continue
			}
			name, _ := splitFirst(strings.TrimPrefix(text, "fabrik:"))
			ds.Warn(fset.Position(c.Slash),
				fmt.Sprintf("//fabrik:%s is not attached to a declaration and is ignored", name),
				"make it the doc comment of a function, type, or variable, with no blank line in between")
		}
	}
	return anns, ds
}

// argsOffset returns the byte offset of the argument text.
func argsOffset(comment, name, args string) int {
	if args == "" {
		return len(comment)
	}
	base := strings.Index(comment, "fabrik:")
	if base < 0 {
		return len(comment)
	}
	i := base + len("fabrik:") + len(name)
	for i < len(comment) && (comment[i] == ' ' || comment[i] == '\t') {
		i++
	}
	return i
}

// warnMainDirectives reports directives in package main as ignored.
func warnMainDirectives(pkg *packages.Package, ds *diag.Diagnostics) {
	for _, file := range pkg.Syntax {
		anns, sds := ScanFile(pkg.Fset, file)
		*ds = append(*ds, sds...)
		for _, ann := range anns {
			ds.Warn(ann.Pos,
				fmt.Sprintf("//fabrik:%s in package main is ignored", ann.Name),
				"move it to a subpackage")
		}
	}
}

// reportPkgErrors drops duplicates and unpositioned summaries.
func selectMain(mains []*packages.Package) (string, error) {
	dirOf := func(pkg *packages.Package) string {
		if len(pkg.GoFiles) == 0 {
			return ""
		}
		return filepath.Dir(pkg.GoFiles[0])
	}
	switch len(mains) {
	case 0:
		return "", nil
	case 1:
		return dirOf(mains[0]), nil
	}
	var candidates []string
	for _, pkg := range mains {
		if callsRun(pkg) {
			candidates = append(candidates, dirOf(pkg))
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	var all []string
	for _, pkg := range mains {
		all = append(all, dirOf(pkg))
	}
	return "", fmt.Errorf("multiple package main found (%s); exactly one must call run()", strings.Join(all, ", "))
}

// callsRun reports whether the package's source calls run() anywhere.
func callsRun(pkg *packages.Package) bool {
	found := false
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "run" {
					found = true
					return false
				}
			}
			return !found
		})
	}
	return found
}

func reportPkgErrors(errs []packages.Error, ds *diag.Diagnostics) {
	positioned := false
	for _, e := range errs {
		if e.Pos != "" && e.Pos != "-" {
			positioned = true
		}
	}
	seen := map[string]bool{}
	for _, e := range errs {
		if positioned && (e.Pos == "" || e.Pos == "-") {
			continue
		}
		key := e.Pos + "|" + e.Msg
		if seen[key] {
			continue
		}
		seen[key] = true
		ds.Error(errorPosition(e), e.Msg, "")
	}
}

// errorPosition parses packages.Error positions from the right.
func errorPosition(e packages.Error) token.Position {
	pos := token.Position{Filename: e.Pos}
	rest := e.Pos
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return pos
	}
	last, err := strconv.Atoi(rest[i+1:])
	if err != nil {
		return pos
	}
	if j := strings.LastIndexByte(rest[:i], ':'); j >= 0 {
		if line, err := strconv.Atoi(rest[j+1 : i]); err == nil {
			return token.Position{Filename: rest[:j], Line: line, Column: last}
		}
	}
	return token.Position{Filename: rest[:i], Line: last}
}

func splitFirst(s string) (head, rest string) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i:])
}

// maybeTypoWarning reports near-miss directive prefixes.
func maybeTypoWarning(pos token.Position, text string, ds *diag.Diagnostics) {
	word := leadingIdent(text)
	if word == "" || word == "fabrik" {
		return
	}
	rest := text[len(word)+1:]
	if strings.EqualFold(word, "fabrik") {
		ds.Warn(pos, fmt.Sprintf("directive prefix %q has wrong case", word),
			"use lowercase: //fabrik:"+rest)
		return
	}
	if d := levenshtein(strings.ToLower(word), "fabrik"); d > 0 && d <= 2 {
		ds.Warn(pos, fmt.Sprintf("suspicious directive prefix %q (did you mean \"fabrik:\"?)", word+":"),
			"example: //fabrik:"+rest)
	}
}

// leadingIdent returns the leading ASCII identifier of text if immediately
// followed by ":", else "".
func leadingIdent(text string) string {
	end := 0
	for end < len(text) {
		b := text[end]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') {
			end++
			continue
		}
		break
	}
	if end == 0 || end >= len(text) || text[end] != ':' {
		return ""
	}
	return text[:end]
}

func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			c := curr[j-1] + 1
			if prev[j]+1 < c {
				c = prev[j] + 1
			}
			if prev[j-1]+cost < c {
				c = prev[j-1] + cost
			}
			curr[j] = c
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}
