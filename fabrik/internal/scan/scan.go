// Package scan reads a project's fabrik directives into the model codegen consumes.
package scan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/gofabrik/fabrik/fabrik/internal/diag"
)

// Project is the scanned view of a fabrik project.
type Project struct {
	ModulePath string
	Root       string
	MainDir    string // directory of package main, where main.gen.go is written
	Packages   []*Package
}

// Package is one scanned Go package.
type Package struct {
	Name       string
	Dir        string
	ImportPath string
	Pos        token.Position // package clause of the first scanned file
	Routes     []*Route
	Providers  []*Provider
	Structs    map[string][]Param // struct name -> fields, for receiver injection
}

// Param is a named, typed function parameter or struct field.
type Param struct {
	Name string
	Type string
	Pos  token.Position
}

// Route is a //fabrik:http directive.
type Route struct {
	Method   string
	Path     string
	Func     string
	Receiver string // e.g. "*Handlers"; empty for a plain function
	Pos      token.Position
}

// Provider is a //fabrik:provider directive.
type Provider struct {
	Func    string
	Returns string
	Params  []Param
	Pos     token.Position
}

var knownDirectives = []string{"http", "provider"}

var knownHTTPMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true,
	"DELETE": true, "HEAD": true, "OPTIONS": true,
}

type scanner struct {
	fset       *token.FileSet
	diags      diag.Diagnostics
	modulePath string
	root       string
	project    *Project
}

// Scan parses the module rooted at dir and returns the scanned Project together
// with any directive diagnostics. The returned error is for I/O problems only;
// directive problems are reported as diagnostics.
func Scan(dir string) (*Project, diag.Diagnostics, error) {
	modulePath, err := readModulePath(dir)
	if err != nil {
		return nil, nil, err
	}

	s := &scanner{
		fset:       token.NewFileSet(),
		modulePath: modulePath,
		root:       dir,
		project:    &Project{ModulePath: modulePath, Root: dir},
	}

	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if path != dir && (strings.HasPrefix(base, ".") || base == "vendor" || base == "node_modules" || base == "testdata") {
			return filepath.SkipDir
		}
		if path != dir {
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir // nested module: not part of this project
			}
		}

		pkg, perr := s.scanPackage(path)
		if perr != nil {
			return perr
		}
		if pkg == nil {
			return nil
		}

		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			pkg.ImportPath = modulePath
		} else {
			pkg.ImportPath = modulePath + "/" + filepath.ToSlash(rel)
		}
		if pkg.Name == "main" {
			s.project.MainDir = path
		}
		s.project.Packages = append(s.project.Packages, pkg)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	s.diags.Sort()
	return s.project, s.diags, nil
}

func readModulePath(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	return "", fmt.Errorf("module path not found in %s/go.mod", dir)
}

func (s *scanner) scanPackage(dir string) (*Package, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	var pkgName string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "main.gen.go" {
			continue
		}
		f, perr := parser.ParseFile(s.fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if perr != nil {
			return nil, fmt.Errorf("parse %s: %w", filepath.Join(dir, name), perr)
		}
		if pkgName == "" {
			pkgName = f.Name.Name
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, nil
	}

	pkg := &Package{Name: pkgName, Dir: dir, Pos: s.fset.Position(files[0].Name.Pos())}
	for _, f := range files {
		s.scanFile(pkg, f)
	}
	return pkg, nil
}

func (s *scanner) scanFile(pkg *Package, f *ast.File) {
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			s.scanFunc(pkg, d)
		case *ast.GenDecl:
			s.scanGenDecl(pkg, d)
		}
	}
}

func (s *scanner) scanFunc(pkg *Package, fn *ast.FuncDecl) {
	if fn.Doc == nil {
		return
	}
	params := extractParams(fn.Type.Params, s.fset)
	receiver := ""
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		receiver = types.ExprString(fn.Recv.List[0].Type)
	}

	for _, c := range fn.Doc.List {
		text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		if !strings.HasPrefix(text, "fabrik:") {
			s.maybeTypoWarning(c.Slash, text)
			continue
		}
		directive, args := splitFirst(strings.TrimPrefix(text, "fabrik:"))
		tokens := tokenize(args)
		switch directive {
		case "http":
			s.parseHTTP(pkg, fn, c, tokens, receiver)
		case "provider":
			s.parseProvider(pkg, fn, c, tokens, params)
		case "":
			s.errHelp(c.Slash, `empty directive after "fabrik:"`,
				"expected one of: "+strings.Join(knownDirectives, ", "))
		default:
			s.errHelp(c.Slash, fmt.Sprintf("unknown directive %q", "fabrik:"+directive),
				"known: "+strings.Join(knownDirectives, ", "))
		}
	}
}

func (s *scanner) parseHTTP(pkg *Package, fn *ast.FuncDecl, c *ast.Comment, tokens []string, receiver string) {
	if len(tokens) == 0 {
		s.errHelp(c.Slash, "//fabrik:http requires METHOD and PATH",
			"example: //fabrik:http GET /login")
		return
	}
	if len(tokens) == 1 {
		s.errHelp(c.Slash, fmt.Sprintf("//fabrik:http requires a PATH after METHOD (got only %q)", tokens[0]),
			"example: //fabrik:http GET /login")
		return
	}
	method, path := tokens[0], tokens[1]
	if !knownHTTPMethods[method] {
		s.errHelp(c.Slash, fmt.Sprintf("unknown HTTP method %q", method),
			"valid methods: GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS")
		return
	}
	if !strings.HasPrefix(path, "/") {
		s.errHelp(c.Slash, fmt.Sprintf("route path must start with %q (got %q)", "/", path),
			"example: //fabrik:http GET /login")
		return
	}
	if len(tokens) > 2 {
		s.errHelp(c.Slash, fmt.Sprintf("unexpected argument %q", tokens[2]),
			"//fabrik:http takes only METHOD and PATH")
		return
	}
	pkg.Routes = append(pkg.Routes, &Route{
		Method:   method,
		Path:     path,
		Func:     fn.Name.Name,
		Receiver: receiver,
		Pos:      s.fset.Position(c.Slash),
	})
}

func (s *scanner) parseProvider(pkg *Package, fn *ast.FuncDecl, c *ast.Comment, tokens []string, params []Param) {
	if len(tokens) > 0 {
		s.errHelp(c.Slash, fmt.Sprintf("//fabrik:provider takes no arguments (got %q)", tokens[0]),
			"example: //fabrik:provider")
		return
	}
	if fn.Type.Results == nil || len(fn.Type.Results.List) == 0 {
		s.errHelp(c.Slash, fmt.Sprintf("//fabrik:provider requires a return value (func %s returns nothing)", fn.Name.Name),
			"example: func NewGreeter() *Greeter")
		return
	}
	pkg.Providers = append(pkg.Providers, &Provider{
		Func:    fn.Name.Name,
		Returns: types.ExprString(fn.Type.Results.List[0].Type),
		Params:  params,
		Pos:     s.fset.Position(c.Slash),
	})
}

func (s *scanner) scanGenDecl(pkg *Package, gd *ast.GenDecl) {
	for _, spec := range gd.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			continue
		}
		if pkg.Structs == nil {
			pkg.Structs = map[string][]Param{}
		}
		pkg.Structs[ts.Name.Name] = extractParams(st.Fields, s.fset)
	}
}

// --- diagnostics helpers -----------------------------------------------------

func (s *scanner) errHelp(pos token.Pos, msg, help string) {
	s.diags = append(s.diags, diag.Diagnostic{
		Severity: diag.SevError,
		Pos:      s.fset.Position(pos),
		Message:  msg,
		Help:     help,
	})
}

func (s *scanner) warnHelp(pos token.Pos, msg, help string) {
	s.diags = append(s.diags, diag.Diagnostic{
		Severity: diag.SevWarning,
		Pos:      s.fset.Position(pos),
		Message:  msg,
		Help:     help,
	})
}

// maybeTypoWarning warns when a comment's leading word looks almost-but-not-quite
// like "fabrik", so an obvious typo (//fabric:http, //farbik:http) doesn't get
// silently treated as an ordinary comment.
func (s *scanner) maybeTypoWarning(pos token.Pos, text string) {
	word := leadingIdent(text)
	if word == "" || word == "fabrik" {
		return
	}
	rest := text[len(word)+1:] // strip "<word>:"
	if strings.EqualFold(word, "fabrik") {
		s.warnHelp(pos, fmt.Sprintf("directive prefix %q has wrong case", word),
			"use lowercase: //fabrik:"+rest)
		return
	}
	if d := levenshtein(strings.ToLower(word), "fabrik"); d > 0 && d <= 2 {
		s.warnHelp(pos, fmt.Sprintf("suspicious directive prefix %q (did you mean \"fabrik:\"?)", word+":"),
			"example: //fabrik:"+rest)
	}
}

// --- parsing helpers ---------------------------------------------------------

func extractParams(fields *ast.FieldList, fset *token.FileSet) []Param {
	if fields == nil {
		return nil
	}
	var out []Param
	for _, field := range fields.List {
		typeStr := types.ExprString(field.Type)
		if len(field.Names) == 0 {
			out = append(out, Param{Type: typeStr, Pos: fset.Position(field.Pos())})
			continue
		}
		for _, name := range field.Names {
			out = append(out, Param{Name: name.Name, Type: typeStr, Pos: fset.Position(name.Pos())})
		}
	}
	return out
}

// tokenize splits a directive's argument string on whitespace, honoring
// double-quoted spans.
func tokenize(s string) []string {
	var tokens []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
		case unicode.IsSpace(r) && !inQuote:
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

func splitFirst(s string) (head, rest string) {
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i:])
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
