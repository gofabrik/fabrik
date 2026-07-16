// Package manifest validates intra-repository module requirements without workspace resolution.
package manifest

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/gofabrik/fabrik/internal/tools/modset"
)

// Finding describes one module manifest problem.
type Finding struct {
	Module string
	Kind   string // "missing-require" | "wrong-version" | "cycle"
	Detail string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: %s: %s", f.Module, f.Kind, f.Detail)
}

// Analyze returns all manifest findings in stable order.
func Analyze(cfg *modset.Config) ([]Finding, error) {
	var findings []Finding

	graph := map[string]map[string]bool{}
	for path := range cfg.Modules {
		graph[path] = map[string]bool{}
	}

	for path, dir := range cfg.Modules {
		imports, err := intraImports(cfg, path, dir)
		if err != nil {
			return nil, err
		}
		reqs, err := intraRequires(cfg, dir)
		if err != nil {
			return nil, err
		}
		for dep := range reqs {
			graph[path][dep] = true
		}
		for dep := range imports {
			if _, ok := reqs[dep]; !ok {
				findings = append(findings, Finding{
					Module: path, Kind: "missing-require",
					Detail: fmt.Sprintf("imports %s but does not require it", dep),
				})
			}
		}
		for dep, ver := range reqs {
			if ver != cfg.Version {
				findings = append(findings, Finding{
					Module: path, Kind: "wrong-version",
					Detail: fmt.Sprintf("requires %s at %s, want %s", dep, ver, cfg.Version),
				})
			}
		}
	}

	if cycle := findCycle(graph); cycle != nil {
		findings = append(findings, Finding{
			Module: cycle[0], Kind: "cycle",
			Detail: "intra-repo module cycle: " + strings.Join(cycle, " -> "),
		})
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Module != findings[j].Module {
			return findings[i].Module < findings[j].Module
		}
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Detail < findings[j].Detail
	})
	return findings, nil
}

// Fix adds missing requirements, normalizes versions, and rejects the resulting graph if cyclic.
func Fix(cfg *modset.Config) ([]string, error) {
	type state struct {
		dir        string
		have, want map[string]string
	}
	states := map[string]*state{}
	desired := map[string]map[string]bool{}
	for path, dir := range cfg.Modules {
		imports, err := intraImports(cfg, path, dir)
		if err != nil {
			return nil, err
		}
		reqs, err := intraRequires(cfg, dir)
		if err != nil {
			return nil, err
		}
		want := map[string]string{}
		for dep := range reqs {
			// Preserve declared test or indirect dependencies while normalizing versions.
			want[dep] = cfg.Version
		}
		for dep := range imports {
			want[dep] = cfg.Version
		}
		states[path] = &state{dir: dir, have: reqs, want: want}
		set := map[string]bool{}
		for dep := range want {
			set[dep] = true
		}
		desired[path] = set
	}

	if cycle := findCycle(desired); cycle != nil {
		return nil, fmt.Errorf("intra-repo module cycle: %s", strings.Join(cycle, " -> "))
	}

	var changed []string
	for path, st := range states {
		did, err := writeRequires(st.dir, st.have, st.want)
		if err != nil {
			return nil, err
		}
		if did {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

func writeRequires(dir string, have, want map[string]string) (bool, error) {
	gomod := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(gomod)
	if err != nil {
		return false, err
	}
	mf, err := modfile.Parse(gomod, data, nil)
	if err != nil {
		return false, err
	}
	dirty := false
	for dep, ver := range want {
		if have[dep] != ver {
			if err := mf.AddRequire(dep, ver); err != nil {
				return false, err
			}
			dirty = true
		}
	}
	if !dirty {
		return false, nil
	}
	mf.Cleanup()
	out, err := mf.Format()
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(gomod, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func intraRequires(cfg *modset.Config, dir string) (map[string]string, error) {
	gomod := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(gomod)
	if err != nil {
		return nil, err
	}
	mf, err := modfile.Parse(gomod, data, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range mf.Require {
		if cfg.Modules[r.Mod.Path] != "" || cfg.Published[r.Mod.Path] {
			out[r.Mod.Path] = r.Mod.Version
		}
	}
	return out, nil
}

// intraImports includes tests but excludes nested modules and self-imports.
func intraImports(cfg *modset.Config, self, dir string) (map[string]bool, error) {
	out := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir {
				if _, e := os.Stat(filepath.Join(p, "go.mod")); e == nil {
					return fs.SkipDir
				}
			}
			if d.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, e := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if e != nil {
			return fmt.Errorf("parse %s: %w", p, e)
		}
		for _, imp := range f.Imports {
			ip, e := strconv.Unquote(imp.Path.Value)
			if e != nil {
				continue
			}
			owner := owningModule(cfg, ip)
			if owner != "" && owner != self {
				out[owner] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// owningModule uses the longest matching module path.
func owningModule(cfg *modset.Config, ip string) string {
	best := ""
	for m := range cfg.Modules {
		if ip == m || strings.HasPrefix(ip, m+"/") {
			if len(m) > len(best) {
				best = m
			}
		}
	}
	return best
}

func findCycle(graph map[string]map[string]bool) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var dfs func(n string) []string
	dfs = func(n string) []string {
		color[n] = gray
		stack = append(stack, n)
		deps := make([]string, 0, len(graph[n]))
		for d := range graph[n] {
			deps = append(deps, d)
		}
		sort.Strings(deps)
		for _, d := range deps {
			switch color[d] {
			case white:
				if c := dfs(d); c != nil {
					return c
				}
			case gray:
				for i, s := range stack {
					if s == d {
						return append(append([]string{}, stack[i:]...), d)
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return nil
	}
	nodes := make([]string, 0, len(graph))
	for n := range graph {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		if color[n] == white {
			if c := dfs(n); c != nil {
				return c
			}
		}
	}
	return nil
}
