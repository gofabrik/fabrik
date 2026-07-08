// Package engine runs directive loading, validation, emission, and rendering.
package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/fabrik/internal/load"
	"github.com/gofabrik/fabrik/gen"
)

// Result of a generation run. Src is nil when the diagnostics contain a
// fatal error.
type Result struct {
	Src     []byte
	MainDir string
	Diags   diag.Diagnostics
}

// Wire generates main.gen.go for the module rooted at dir.
// Overlay contents replace on-disk files during loading.
func Wire(dir string, overlay map[string][]byte) (*Result, error) {
	res, err := load.Load(dir, overlay)
	if err != nil {
		return nil, err
	}
	if res.MainDir == "" {
		return nil, fmt.Errorf("no package main found under %s", res.Root)
	}

	directives := New()
	byName := map[string]gen.Directive{}
	names := make([]string, 0, len(directives))
	for _, d := range directives {
		byName[d.Name()] = d
		names = append(names, d.Name())
	}
	sort.Strings(names)

	diags := res.Diags
	var parsed []gen.Parsed
	for _, item := range res.Items {
		d, ok := byName[item.Ann.Name]
		if !ok {
			if item.Ann.Name == "" {
				diags.Error(item.Ann.Pos, `empty directive after "fabrik:"`,
					"expected one of: "+strings.Join(names, ", "))
			} else {
				diags.Error(item.Ann.Pos, fmt.Sprintf("unknown directive %q", "fabrik:"+item.Ann.Name),
					"known: "+strings.Join(names, ", "))
			}
			continue
		}
		node, ds := d.Parse(item.Ann)
		diags = append(diags, ds...)
		if node == nil || ds.HasFatal() {
			continue
		}
		ds = d.Check(node, item.Typed)
		diags = append(diags, ds...)
		if ds.HasFatal() {
			continue
		}
		parsed = append(parsed, gen.Parsed{Directive: d, Node: node})
	}

	// Emit only after Parse and Check succeed across the project.
	if diags.HasFatal() {
		diags.Sort()
		return &Result{MainDir: res.MainDir, Diags: diags}, nil
	}

	g := gen.New()
	for tier := tierProvider; tier <= tierRest; tier++ {
		for _, p := range parsed {
			if emitTier(p.Directive) != tier {
				continue
			}
			g.SetDirective(p.Directive.Name())
			diags = append(diags, p.Directive.Emit(p.Node, g)...)
		}
	}
	for _, d := range directives {
		if f, ok := d.(gen.Finisher); ok {
			g.SetDirective(d.Name())
			diags = append(diags, f.Finish(g)...)
		}
	}
	diags.Sort()
	if diags.HasFatal() {
		return &Result{MainDir: res.MainDir, Diags: diags}, nil
	}

	src, err := g.Render()
	if err != nil {
		return nil, err
	}
	return &Result{Src: src, MainDir: res.MainDir, Diags: diags}, nil
}

// Emission tiers preserve dependencies between directive kinds.
const (
	tierProvider = iota
	tierInit
	tierRest
)

func emitTier(d gen.Directive) int {
	switch d.Name() {
	case "provider":
		return tierProvider
	case "init":
		return tierInit
	default:
		return tierRest
	}
}
