package engine

import (
	"testing"

	"github.com/gofabrik/fabrik/fabrik/internal/genconfig"
	"github.com/gofabrik/fabrik/gen"
)

func TestOptionsFromMapsEveryEngineField(t *testing.T) {
	cfg := genconfig.Options{Comments: gen.CommentsFull, BuildTag: "wired", Split: genconfig.SplitFragment}
	o := OptionsFrom(cfg)
	if o.Comments != gen.CommentsFull || o.BuildTag != "wired" || !o.Split {
		t.Fatalf("OptionsFrom = %+v", o)
	}
}
