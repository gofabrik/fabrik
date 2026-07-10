package core

import (
	"go/types"

	cfgdir "github.com/gofabrik/fabrik/config/directive"
	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// resolveArgs turns parameters into call arguments under a caller policy.
// Diagnostics from accept are complete and suppress the generic rejection.
func resolveArgs(g *gen.Gen, cfg *cfgdir.Config, params []param,
	accept func(pr param) (string, diag.Diagnostics, bool),
	reject func(pr param) (msg, help string)) ([]string, diag.Diagnostics) {

	var ds diag.Diagnostics
	args := make([]string, 0, len(params))
	for _, pr := range params {
		if gen.IsContext(pr.t) {
			args = append(args, g.Context())
			continue
		}
		expr, eds, ok := accept(pr)
		ds = append(ds, anchor(eds, pr.pos)...)
		if ok {
			args = append(args, expr)
			continue
		}
		if len(eds) == 0 {
			msg, help := reject(pr)
			ds.Error(pr.pos, msg, missingHelp(g, cfg, pr.t, help))
		}
		args = append(args, "nil")
	}
	return args, ds
}

// missingHelp returns a domain hint when one applies: directive-owned
// hints registered on the generator first, config's own second.
func missingHelp(g *gen.Gen, cfg *cfgdir.Config, t types.Type, def string) string {
	if h, ok := g.MissingHint(t); ok {
		return h
	}
	if h, ok := cfg.MissingHint(t); ok {
		return h
	}
	return def
}
