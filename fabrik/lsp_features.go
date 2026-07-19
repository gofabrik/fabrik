package main

import (
	"go/parser"
	"go/token"
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
	ds = append(ds, engine.SyntaxDiags(anns)...)

	s.notify("textDocument/publishDiagnostics", publishDiagnosticsParams{
		URI:         uri,
		Diagnostics: lspDiagnostics(ds, map[string]string{path: text}),
	})
}

// publishTyped reports workspace diagnostics and clears stale ones.
func (s *lspServer) publishTyped(uri string, gen int) {
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
	// A newer edit may have scheduled a fresh publish while this Wire ran;
	// publishing now would overwrite current diagnostics with stale ones.
	s.mu.Lock()
	stale := s.pubGen[root] != gen
	s.mu.Unlock()
	if stale {
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

	srcCache := map[string]string{}
	srcFor := func(path string) string {
		if t, ok := srcCache[path]; ok {
			return t
		}
		t := sourceFor(path, texts)
		srcCache[path] = t
		return t
	}
	for _, d := range res.Diags {
		byFile[d.Pos.Filename] = append(byFile[d.Pos.Filename], lspDiagnostic{
			Range:    spanRange(srcFor(d.Pos.Filename), d.Pos),
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

	lineText := nthLine(src, line)
	if startCol < len(lineText) {
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

	// LSP columns are UTF-16 units; token positions are byte offsets.
	return lspRange{
		Start: lspPosition{Line: line, Character: utf16Col(lineText, startCol)},
		End:   lspPosition{Line: line, Character: utf16Col(lineText, endCol)},
	}
}

func utf16Col(line string, byteOff int) int {
	if byteOff > len(line) {
		byteOff = len(line)
	}
	n := 0
	for _, r := range line[:byteOff] {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func byteCol(line string, utf16Off int) int {
	n := 0
	for i, r := range line {
		if n >= utf16Off {
			return i
		}
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return len(line)
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
	cursor := byteCol(line, pos.Character)

	// Preserve trailing space to distinguish current and next token.
	left := strings.TrimLeft(line[:cursor], " \t")
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
	if partial, ok := activeMWChain(meta, args, trailingSpace); ok {
		return s.middlewareCompletions(uri, partial)
	}
	return argCompletions(meta, args, trailingSpace)
}

func activeMWChain(meta gen.Meta, args []string, trailingSpace bool) (string, bool) {
	for i := len(args) - 1; i >= 0; i-- {
		key, val, hasEq := strings.Cut(args[i], "=")
		if !hasEq {
			continue
		}
		if !isMWAttr(meta, key) {
			return "", false
		}
		cur := val
		for j := i + 1; j < len(args); j++ {
			if !strings.HasSuffix(cur, ",") {
				return "", false
			}
			cur = args[j]
		}
		if !trailingSpace {
			return cur, true
		}
		if strings.HasSuffix(cur, ",") {
			return "", true
		}
		return "", false
	}
	return "", false
}

func isMWAttr(meta gen.Meta, key string) bool {
	for _, spec := range meta.Attrs {
		if spec.Key == key {
			return spec.Kind == gen.KindMiddlewareRef
		}
	}
	return false
}

func (s *lspServer) middlewareCompletions(uri, partial string) []completionItem {
	if i := strings.LastIndex(partial, ","); i >= 0 {
		partial = partial[i+1:]
	}
	root := s.rootForURI(uri)
	if root == "" {
		return nil
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
		for _, name := range s.middlewareNames(path, overlay[path], d) {
			if seen[name] || (partial != "" && !strings.HasPrefix(name, partial)) {
				continue
			}
			seen[name] = true
			out = append(out, completionItem{Label: name, Kind: 3, Detail: "middleware", InsertText: name})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

type mwCacheEntry struct {
	size    int64
	modTime int64
	names   []string
}

// mwDirective keeps completion parsing aligned with generation.
var mwDirective = func() gen.Directive {
	for _, d := range engine.New() {
		if d.Name() == "http:middleware" {
			return d
		}
	}
	return nil
}()

func (s *lspServer) middlewareNames(path, overlayText string, d iofs.DirEntry) []string {
	var info iofs.FileInfo
	if overlayText == "" {
		if fi, err := d.Info(); err == nil {
			info = fi
			s.mu.Lock()
			entry, hit := s.mwCache[path]
			s.mu.Unlock()
			if hit && entry.size == fi.Size() && entry.modTime == fi.ModTime().UnixNano() {
				return entry.names
			}
		}
	}

	var src any
	if overlayText != "" {
		src = overlayText
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	var names []string
	if err == nil && f.Name.Name != "main" {
		anns, _ := load.ScanFile(fset, f)
		for _, ann := range anns {
			if ann.Name != "http:middleware" || mwDirective == nil {
				continue
			}
			if _, ds := mwDirective.Parse(ann); ds.HasFatal() {
				continue
			}
			args, _ := gen.ParseArgs(ann, mwDirective.Meta())
			if nm, ok := args.Attr["name"]; ok && nm.Text != "" {
				names = append(names, nm.Text)
			}
		}
	}
	if info != nil {
		s.mu.Lock()
		s.mwCache[path] = mwCacheEntry{size: info.Size(), modTime: info.ModTime().UnixNano(), names: names}
		s.mu.Unlock()
	}
	return names
}

func directiveCompletions(directives []gen.Directive, partial string) []completionItem {
	var out []completionItem
	for _, d := range directives {
		if d.Meta().Hidden {
			continue
		}
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
	col := byteCol(line, pos.Character) - nameStart
	nameEnd := strings.IndexAny(body, " \t")
	if nameEnd < 0 {
		nameEnd = len(body)
	}
	if col < 0 || col > nameEnd {
		return nil
	}
	name := body[:nameEnd]
	for _, d := range engine.New() {
		if d.Name() != name || d.Meta().Hidden {
			continue
		}
		return hoverResult{
			Contents: markupContent{Kind: "markdown", Value: d.Meta().Doc},
			Range: &lspRange{
				Start: lspPosition{Line: pos.Line, Character: utf16Col(line, nameStart)},
				End:   lspPosition{Line: pos.Line, Character: utf16Col(line, nameStart+nameEnd)},
			},
		}
	}
	return nil
}

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
