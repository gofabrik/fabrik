package gen

import (
	"fmt"
	"sort"

	"github.com/gofabrik/fabrik/diag"
)

// Demand-usage bookkeeping: every demand a flow makes on a lazy
// binding, a path binding, or a scope callback records the demanding
// flow. The resulting usage signatures drive fragment planning; the
// validation pass uses them to sweep only bindings no flow reached.

type demandKind uint8

const (
	demandType demandKind = iota
	demandPath
	demandCallback
)

// demandKey identifies one demand: a (type, name) lazy binding, a path
// binding, or a scope callback by registration ordinal.
type demandKey struct {
	kind demandKind
	key  string // TypeString for demandType (plus name), path for demandPath
	name string
	ord  int // callback registration ordinal
}

// flowID names the active flow: the scope's build function, "default"
// outside any scope, "" in the validation scope (not a flow).
func (g *Gen) flowID() string {
	if sc := g.scope; sc != nil {
		if sc.validation {
			return ""
		}
		return sc.fn
	}
	return "default"
}

func (g *Gen) recordDemand(k demandKey) {
	flow := g.flowID()
	if flow == "" {
		return
	}
	if g.demands == nil {
		g.demands = map[demandKey]map[string]bool{}
	}
	m := g.demands[k]
	if m == nil {
		m = map[string]bool{}
		g.demands[k] = m
	}
	m[flow] = true
}

// demandFlows returns the sorted flows that demanded k.
func (g *Gen) demandFlows(k demandKey) []string {
	m := g.demands[k]
	if len(m) == 0 {
		return nil
	}
	flows := make([]string, 0, len(m))
	for f := range m {
		flows = append(flows, f)
	}
	sort.Strings(flows)
	return flows
}

// demanded reports whether any flow reached k.
func (g *Gen) demanded(k demandKey) bool { return len(g.demands[k]) > 0 }

// reportCallbackDiags filters one epilogue callback's diagnostics to
// distinct occurrences across every scope and the validation replay:
// the flow that discovers a diagnostic reports it, identical repeats
// from other flows collapse, matching the single report the
// validation-first order produced. Identity spans the callback
// ordinal and the full diagnostic, so distinct callbacks or
// severities never collapse.
func (g *Gen) reportCallbackDiags(ord int, ds diag.Diagnostics) diag.Diagnostics {
	if len(ds) == 0 {
		return nil
	}
	if g.callbackSeen == nil {
		g.callbackSeen = map[string]bool{}
	}
	var out diag.Diagnostics
	for _, d := range ds {
		key := fmt.Sprintf("%d\x00%d\x00%s\x00%s\x00%s", ord, d.Severity, d.Message, d.Pos, d.Help)
		if g.callbackSeen[key] {
			continue
		}
		g.callbackSeen[key] = true
		out = append(out, d)
	}
	return out
}
