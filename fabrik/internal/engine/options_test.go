package engine

import (
	"go/token"
	"testing"

	"github.com/gofabrik/fabrik/fabrik/internal/genconfig"
	"github.com/gofabrik/fabrik/gen"
)

func TestOptionsFromMapsEveryEngineField(t *testing.T) {
	pos := token.Position{Filename: "fabrik.yaml", Line: 2, Column: 3}
	cfg := genconfig.Options{
		Comments:      gen.CommentsFull,
		BuildTag:      "wired",
		Split:         genconfig.SplitFragment,
		Emit:          genconfig.EmitEmbedded,
		Dir:           "appwire",
		Package:       "appwire",
		Entrypoints:   []string{"sync"},
		EmitPos:       pos,
		EntrypointPos: map[string]token.Position{"sync": pos},
	}
	o := OptionsFrom(cfg)
	if o.Comments != gen.CommentsFull || o.BuildTag != "wired" || !o.Split ||
		!o.Embedded || o.Dir != "appwire" || o.Package != "appwire" ||
		len(o.Entrypoints) != 1 || o.EmitPos != pos || o.EntrypointPos["sync"] != pos {
		t.Fatalf("OptionsFrom = %+v", o)
	}
}
