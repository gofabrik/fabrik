package core

import (
	"go/types"

	cfgdir "github.com/gofabrik/fabrik/config/directive"
	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

// resolveArgs turns parameters into call arguments under a site policy:
// context.Context resolves to the shared background context, accept
// resolves the site's supported types, and anything else is rejected
// with the site's diagnostic. Diagnostics returned by accept explain the
// failure themselves (e.g. a provider cycle), so no rejection is added
// on top of them.
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
			ds.Error(pr.pos, msg, missingHelp(cfg, pr.t, help))
		}
		args = append(args, "nil")
	}
	return args, ds
}

// missingHelp returns the config directive's hint for t when one applies
// (the take-a-pointer near-miss), else the site's default help.
func missingHelp(cfg *cfgdir.Config, t types.Type, def string) string {
	if h, ok := cfg.MissingHint(t); ok {
		return h
	}
	return def
}
