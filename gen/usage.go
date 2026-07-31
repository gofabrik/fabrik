package gen

import (
	"fmt"
	"sort"

	"github.com/gofabrik/fabrik/diag"
)

// Demand usage records which flows reach lazy bindings, path bindings, and scope callbacks.
// Fragment planning and validation use the resulting flow signatures.

type demandKind uint8

const (
	demandType demandKind = iota
	demandPath
	demandCallback
	demandSingleton
)

// demandKey identifies a lazy binding, path binding, singleton, or scope callback.
type demandKey struct {
	kind demandKind
	key  string // TypeString for demandType (plus name), path for demandPath
	name string
	ord  int // callback registration ordinal
}

// flowID returns the scope build function, "default", or "" during validation.
func (g *Gen) flowID() string {
	if sc := g.scope; sc != nil {
		if sc.validation {
			return ""
		}
		return sc.fn
	}
	if g.frag != nil {
		if g.frag.flow != "" {
			return g.frag.flow
		}
		if g.frag.noFlow {
			return ""
		}
	}
	return "default"
}

func (g *Gen) recordDemand(k demandKey) {
	if g.frag != nil {
		if parent := g.frag.currentDemand(); parent >= 0 {
			pk := g.frag.demandOrder[parent]
			if pk != k {
				if g.demandEdges == nil {
					g.demandEdges = map[demandKey]map[demandKey]bool{}
				}
				if g.demandEdges[pk] == nil {
					g.demandEdges[pk] = map[demandKey]bool{}
				}
				g.demandEdges[pk][k] = true
			}
		}
	}
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

// propagateDemandUsage spreads flow usage through demand edges to a fixed point.
func (g *Gen) propagateDemandUsage() {
	changed := true
	for changed {
		changed = false
		for parent, children := range g.demandEdges {
			pf := g.demands[parent]
			if len(pf) == 0 {
				continue
			}
			for child := range children {
				cf := g.demands[child]
				if cf == nil {
					cf = map[string]bool{}
					g.demands[child] = cf
				}
				for f := range pf {
					if !cf[f] {
						cf[f] = true
						changed = true
					}
				}
			}
		}
	}
}

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

func (g *Gen) demanded(k demandKey) bool { return len(g.demands[k]) > 0 }

// SingletonDemanded reports demand in the active flow, or across all flows outside a flow.
func (g *Gen) SingletonDemanded(key string) bool {
	m := g.demands[demandKey{kind: demandSingleton, key: key}]
	if len(m) == 0 {
		return false
	}
	if f := g.flowID(); f != "" {
		return m[f]
	}
	return true
}

// reportCallbackDiags deduplicates identical diagnostics from one callback across flow replays.
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
