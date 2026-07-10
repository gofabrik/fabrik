package assetmapper

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// assetKind selects the reference scanner for an asset.
type assetKind int

const (
	kindOther assetKind = iota
	kindJS
	kindCSS
)

func kindOf(logicalPath string) assetKind {
	switch path.Ext(logicalPath) {
	case ".js", ".mjs":
		return kindJS
	case ".css":
		return kindCSS
	}
	return kindOther
}

// ref is one source reference that may need a hashed URL.
type ref struct {
	// spec is the captured source specifier.
	spec string
	// resolved is empty for external URLs, bare specifiers, and paths outside the asset root.
	resolved string
	// suffix preserves ?query and #fragment after path resolution.
	suffix string
	// start and end bracket the specifier text without surrounding quotes.
	start, end int
}

// jsImportRE matches static and dynamic import/export specifiers.
//
// False positives are left unchanged unless they resolve to a known asset.
var jsImportRE = regexp.MustCompile(
	`\b(?:import|export)\b[^'";]*?["']([^"'\n]+)["']`,
)

// cssURLRE matches double-quoted, single-quoted, and unquoted url(...) references.
var cssURLRE = regexp.MustCompile(
	`url\(\s*(?:"([^"]+)"|'([^']+)'|([^)\s'"]+))\s*\)`,
)

// cssImportRE matches @import "..." or @import '...' (the bare-string
// form). @import url("...") is covered by [cssURLRE] already.
var cssImportRE = regexp.MustCompile(
	`@import\s+["']([^"'\n]+)["']`,
)

// extractRefs returns rewrite candidates with byte ranges.
func extractRefs(importerPath string, content []byte, kind assetKind) []ref {
	var refs []ref
	switch kind {
	case kindJS:
		for _, m := range jsImportRE.FindAllSubmatchIndex(content, -1) {
			refs = append(refs, refAt(importerPath, content, m[2], m[3]))
		}
	case kindCSS:
		for _, m := range cssURLRE.FindAllSubmatchIndex(content, -1) {
			s, e := pickAlternation(m, 2, 4, 6)
			if s < 0 {
				continue
			}
			refs = append(refs, refAt(importerPath, content, s, e))
		}
		for _, m := range cssImportRE.FindAllSubmatchIndex(content, -1) {
			refs = append(refs, refAt(importerPath, content, m[2], m[3]))
		}
	}
	return refs
}

func refAt(importer string, content []byte, start, end int) ref {
	spec := string(content[start:end])
	suffix := ""
	if i := strings.IndexAny(spec, "?#"); i >= 0 {
		suffix = spec[i:]
	}
	return ref{
		spec:     spec,
		resolved: resolveRef(importer, spec),
		suffix:   suffix,
		start:    start,
		end:      end,
	}
}

// pickAlternation returns the first present regex submatch range.
func pickAlternation(indices []int, groupStarts ...int) (int, int) {
	for _, gs := range groupStarts {
		if gs < len(indices) && indices[gs] >= 0 {
			return indices[gs], indices[gs+1]
		}
	}
	return -1, -1
}

// resolveRef converts a local source reference into a logical asset path.
//
// It returns "" for external URLs, bare specifiers, fragments, and paths outside the asset root.
func resolveRef(importerPath, spec string) string {
	if spec == "" {
		return ""
	}
	// Strip ?query and #fragment for resolution.
	if i := strings.IndexAny(spec, "?#"); i >= 0 {
		spec = spec[:i]
	}
	if spec == "" {
		return ""
	}
	if strings.HasPrefix(spec, "http://") ||
		strings.HasPrefix(spec, "https://") ||
		strings.HasPrefix(spec, "//") ||
		strings.HasPrefix(spec, "data:") {
		return ""
	}
	if strings.HasPrefix(spec, "/") {
		return cleanLogical(spec)
	}
	if strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") {
		dir := path.Dir(importerPath)
		if dir == "." {
			dir = ""
		}
		resolved := path.Join(dir, spec)
		if resolved == "." || strings.HasPrefix(resolved, "../") || resolved == ".." {
			return ""
		}
		return resolved
	}
	// Bare specifiers belong to the importmap.
	return ""
}

// rewriteRefs replaces resolved references and leaves all others unchanged.
func rewriteRefs(content []byte, refs []ref, replacement func(r ref) string) []byte {
	sort.Slice(refs, func(i, j int) bool { return refs[i].start < refs[j].start })

	var out []byte
	cursor := 0
	for _, r := range refs {
		if r.resolved == "" {
			continue
		}
		repl := replacement(r)
		if repl == r.spec {
			continue
		}
		// Overlapping regex matches would corrupt the splice.
		if r.start < cursor {
			continue
		}
		out = append(out, content[cursor:r.start]...)
		out = append(out, repl...)
		cursor = r.end
	}
	out = append(out, content[cursor:]...)
	return out
}

// topoSort orders dependencies before dependents with deterministic tie-breaking.
func topoSort(deps map[string][]string) ([]string, error) {
	dependents := map[string][]string{}
	indegree := map[string]int{}
	for node, ds := range deps {
		if _, ok := indegree[node]; !ok {
			indegree[node] = 0
		}
		for _, dep := range ds {
			dependents[dep] = append(dependents[dep], node)
			indegree[node]++
		}
	}

	var ready []string
	for node := range deps {
		if indegree[node] == 0 {
			ready = append(ready, node)
		}
	}
	sort.Strings(ready)

	order := make([]string, 0, len(deps))
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		var newly []string
		for _, dep := range dependents[n] {
			indegree[dep]--
			if indegree[dep] == 0 {
				newly = append(newly, dep)
			}
		}
		if len(newly) > 0 {
			ready = append(ready, newly...)
			sort.Strings(ready)
		}
	}

	if len(order) != len(deps) {
		var inCycle []string
		for node, deg := range indegree {
			if deg > 0 {
				inCycle = append(inCycle, node)
			}
		}
		sort.Strings(inCycle)
		return nil, &CycleError{Nodes: inCycle}
	}
	return order, nil
}

// CycleError reports assets involved in a dependency cycle.
type CycleError struct {
	Nodes []string
}

func (e *CycleError) Error() string {
	return "assetmapper: dependency cycle among assets: " + strings.Join(e.Nodes, ", ")
}
