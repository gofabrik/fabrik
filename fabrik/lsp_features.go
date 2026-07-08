package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/engine"
	"github.com/gofabrik/fabrik/fabrik/internal/load"
	"github.com/gofabrik/fabrik/gen"
)

// publishSyntactic reports directive grammar errors from the changed file.
func (s *lspServer) publishSyntactic(uri string) {
	text, ok := s.getDoc(uri)
	if !ok {
		return
	}
	path := fileFromURI(uri)
	if path == "" || !strings.HasSuffix(path, ".go") {
		return
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, text, parser.ParseComments)
	if err != nil {
		return
	}

	anns, ds := load.ScanFile(fset, file)
	byName := map[string]gen.Directive{}
	var names []string
	for _, d := range engine.New() {
		byName[d.Name()] = d
		names = append(names, d.Name())
	}
	for _, ann := range anns {
		d, ok := byName[ann.Name]
		if !ok {
			if ann.Name == "" {
				ds.Error(ann.Pos, `empty directive after "fabrik:"`, "expected one of: "+strings.Join(names, ", "))
			} else {
				ds.Error(ann.Pos, "unknown directive \"fabrik:"+ann.Name+"\"", "known: "+strings.Join(names, ", "))
			}
			continue
		}
		_, pds := d.Parse(ann)
		ds = append(ds, pds...)
	}

	s.notify("textDocument/publishDiagnostics", publishDiagnosticsParams{
		URI:         uri,
		Diagnostics: lspDiagnostics(ds, map[string]string{path: text}),
	})
}

// publishTyped reports workspace diagnostics and clears stale ones.
func (s *lspServer) publishTyped(uri string) {
	root := s.rootForURI(uri)
	if root == "" {
		return
	}

	s.mu.Lock()
	overlay := make(map[string][]byte, len(s.docs))
	texts := map[string]string{}
	for u, t := range s.docs {
		if p := fileFromURI(u); p != "" {
			overlay[p] = []byte(t)
			texts[p] = t
		}
	}
	s.mu.Unlock()

	res, err := engine.Wire(root, overlay)
	if err != nil {
		return
	}

	byFile := map[string][]lspDiagnostic{}
	byFile[fileFromURI(uri)] = nil
	s.mu.Lock()
	for u := range s.published[root] {
		if p := fileFromURI(u); p != "" {
			byFile[p] = nil
		}
	}
	s.mu.Unlock()

	for _, d := range res.Diags {
		byFile[d.Pos.Filename] = append(byFile[d.Pos.Filename], lspDiagnostic{
			Range:    spanRange(sourceFor(d.Pos.Filename, texts), d.Pos),
			Severity: lspSeverity(d.Severity),
			Source:   "fabrik",
			Message:  diagText(d),
		})
	}

	current := map[string]bool{}
	for path, list := range byFile {
		if list == nil {
			list = []lspDiagnostic{}
		} else {
			current[uriFromFile(path)] = true
		}
		s.notify("textDocument/publishDiagnostics", publishDiagnosticsParams{
			URI:         uriFromFile(path),
			Diagnostics: list,
		})
	}
	s.mu.Lock()
	s.published[root] = current
	s.mu.Unlock()
}

func lspDiagnostics(ds diag.Diagnostics, texts map[string]string) []lspDiagnostic {
	out := []lspDiagnostic{}
	for _, d := range ds {
		out = append(out, lspDiagnostic{
			Range:    spanRange(sourceFor(d.Pos.Filename, texts), d.Pos),
			Severity: lspSeverity(d.Severity),
			Source:   "fabrik",
			Message:  diagText(d),
		})
	}
	return out
}

func lspSeverity(s diag.Severity) int {
	if s == diag.SevWarning {
		return 2
	}
	return 1
}

func diagText(d diag.Diagnostic) string {
	if d.Help == "" {
		return d.Message
	}
	return d.Message + "\n\nhelp: " + d.Help
}

// sourceFor prefers open-document text over disk.
func sourceFor(path string, texts map[string]string) string {
	if t, ok := texts[path]; ok {
		return t
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// spanRange covers the token at the diagnostic column.
func spanRange(src string, pos token.Position) lspRange {
	line := pos.Line - 1
	startCol := pos.Column - 1
	if startCol < 0 {
		startCol = 0
	}
	endCol := startCol + 1

	if lineText := nthLine(src, line); startCol < len(lineText) {
		end := startCol
		for end < len(lineText) {
			b := lineText[end]
			if b == ' ' || b == '\t' || b == '\r' {
				break
			}
			end++
		}
		endCol = end
	}

	return lspRange{
		Start: lspPosition{Line: line, Character: startCol},
		End:   lspPosition{Line: line, Character: endCol},
	}
}

func nthLine(src string, n int) string {
	lines := strings.Split(src, "\n")
	if n < 0 || n >= len(lines) {
		return ""
	}
	return lines[n]
}

// completion is driven by directive metadata and middleware discovery.
func (s *lspServer) completion(uri string, pos lspPosition) []completionItem {
	text, ok := s.getDoc(uri)
	if !ok {
		return nil
	}
	line := nthLine(text, pos.Line)
	if pos.Character > len(line) {
		pos.Character = len(line)
	}

	// Preserve trailing space to distinguish current and next token.
	left := strings.TrimLeft(line[:pos.Character], " \t")
	if !strings.HasPrefix(left, "//") {
		return nil
	}
	left = strings.TrimLeft(strings.TrimPrefix(left, "//"), " \t")
	if !strings.HasPrefix(left, "fabrik:") {
		return nil
	}
	rest := strings.TrimPrefix(left, "fabrik:")
	trailingSpace := strings.HasSuffix(rest, " ") || strings.HasSuffix(rest, "\t")
	tokens := strings.Fields(rest)

	directives := engine.New()

	if len(tokens) == 0 {
		return directiveCompletions(directives, "")
	}
	if len(tokens) == 1 && !trailingSpace {
		return directiveCompletions(directives, tokens[0])
	}

	var meta gen.Meta
	found := false
	for _, d := range directives {
		if d.Name() == tokens[0] {
			meta, found = d.Meta(), true
			break
		}
	}
	if !found {
		return nil
	}

	args := tokens[1:]
	if !trailingSpace && len(args) > 0 {
		if key, val, ok := strings.Cut(args[len(args)-1], "="); ok {
			for _, spec := range meta.Attrs {
				if spec.Key == key && spec.Kind == gen.KindMiddlewareRef {
					return s.middlewareCompletions(uri, val)
				}
			}
		}
	}
	return argCompletions(meta, args, trailingSpace)
}

// middlewareCompletions offers local names or pkg.Name for middleware funcs.
func (s *lspServer) middlewareCompletions(uri, partial string) []completionItem {
	if i := strings.LastIndex(partial, ","); i >= 0 {
		partial = partial[i+1:]
	}
	root := s.rootForURI(uri)
	if root == "" {
		return nil
	}

	curPkg := ""
	if text, ok := s.getDoc(uri); ok {
		fset := token.NewFileSet()
		if f, err := parser.ParseFile(fset, fileFromURI(uri), text, parser.PackageClauseOnly); err == nil {
			curPkg = f.Name.Name
		}
	}

	s.mu.Lock()
	overlay := map[string]string{}
	for u, t := range s.docs {
		if p := fileFromURI(u); p != "" {
			overlay[p] = t
		}
	}
	s.mu.Unlock()

	seen := map[string]bool{}
	var out []completionItem
	filepath.WalkDir(root, func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := d.Name()
			if path != root && (strings.HasPrefix(base, ".") || base == "vendor" || base == "node_modules" || base == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "main.gen.go" {
			return nil
		}
		var src any
		if t, ok := overlay[path]; ok {
			src = t
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil || f.Name.Name == "main" {
			return nil
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || !isMiddlewareShape(fd.Type) {
				continue
			}
			if !fd.Name.IsExported() && f.Name.Name != curPkg {
				continue
			}
			label := f.Name.Name + "." + fd.Name.Name
			if f.Name.Name == curPkg {
				label = fd.Name.Name
			}
			if seen[label] || (partial != "" && !strings.HasPrefix(label, partial)) {
				continue
			}
			seen[label] = true
			out = append(out, completionItem{Label: label, Kind: 3, Detail: "middleware", InsertText: label})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// isMiddlewareShape recognizes func(http.Handler) http.Handler in source.
func isMiddlewareShape(ft *ast.FuncType) bool {
	count := func(fl *ast.FieldList) int {
		if fl == nil {
			return 0
		}
		n := 0
		for _, f := range fl.List {
			m := len(f.Names)
			if m == 0 {
				m = 1
			}
			n += m
		}
		return n
	}
	return count(ft.Params) == 1 && count(ft.Results) == 1 &&
		types.ExprString(ft.Params.List[0].Type) == "http.Handler" &&
		types.ExprString(ft.Results.List[0].Type) == "http.Handler"
}

func directiveCompletions(directives []gen.Directive, partial string) []completionItem {
	var out []completionItem
	for _, d := range directives {
		if partial != "" && !strings.HasPrefix(d.Name(), partial) {
			continue
		}
		out = append(out, completionItem{
			Label:      d.Name(),
			Detail:     d.Meta().Synopsis,
			Kind:       14, // Keyword
			InsertText: d.Name() + " ",
		})
	}
	return out
}

func argCompletions(meta gen.Meta, args []string, trailingSpace bool) []completionItem {
	cur := len(args)
	partial := ""
	if !trailingSpace && len(args) > 0 {
		cur = len(args) - 1
		partial = args[cur]
	}

	if cur < len(meta.Pos) {
		var out []completionItem
		for _, v := range meta.Pos[cur].Values {
			if partial != "" && !strings.HasPrefix(v, partial) {
				continue
			}
			out = append(out, completionItem{Label: v, Kind: 12, InsertText: v + " "})
		}
		return out
	}

	if key, val, ok := strings.Cut(partial, "="); ok {
		for _, a := range meta.Attrs {
			if a.Key != key {
				continue
			}
			var out []completionItem
			for _, v := range a.Values {
				if val != "" && !strings.HasPrefix(v, val) {
					continue
				}
				out = append(out, completionItem{Label: v, Kind: 12, InsertText: v})
			}
			return out
		}
		return nil
	}

	used := map[string]bool{}
	for _, a := range args {
		if k, _, ok := strings.Cut(a, "="); ok {
			used[k] = true
		}
	}
	var out []completionItem
	for _, a := range meta.Attrs {
		if used[a.Key] {
			continue
		}
		if partial != "" && !strings.HasPrefix(a.Key, partial) {
			continue
		}
		out = append(out, completionItem{Label: a.Key + "=", Kind: 5, InsertText: a.Key + "="})
	}
	return out
}

// hover shows directive docs for the name under the cursor.
func (s *lspServer) hover(uri string, pos lspPosition) any {
	text, ok := s.getDoc(uri)
	if !ok {
		return nil
	}
	line := nthLine(text, pos.Line)
	nameStart, body, isDir := stripDirective(line)
	if !isDir {
		return nil
	}
	col := pos.Character - nameStart
	nameEnd := strings.IndexAny(body, " \t")
	if nameEnd < 0 {
		nameEnd = len(body)
	}
	if col < 0 || col > nameEnd {
		return nil
	}
	name := body[:nameEnd]
	for _, d := range engine.New() {
		if d.Name() != name {
			continue
		}
		return hoverResult{
			Contents: markupContent{Kind: "markdown", Value: d.Meta().Doc},
			Range: &lspRange{
				Start: lspPosition{Line: pos.Line, Character: nameStart},
				End:   lspPosition{Line: pos.Line, Character: nameStart + nameEnd},
			},
		}
	}
	return nil
}

// stripDirective returns a directive body and its start column.
func stripDirective(line string) (int, string, bool) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if !strings.HasPrefix(line[i:], "//") {
		return 0, "", false
	}
	i += 2
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if !strings.HasPrefix(line[i:], "fabrik:") {
		return 0, "", false
	}
	i += len("fabrik:")
	return i, line[i:], true
}
