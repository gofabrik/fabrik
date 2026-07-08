package gen

import (
	"go/token"
	"reflect"
	"testing"
)

func ann(args string) Annotation {
	return Annotation{
		Name:    "test",
		Args:    args,
		Pos:     token.Position{Filename: "x.go", Line: 1, Column: 1},
		ArgsCol: 15,
	}
}

func TestParseArgs(t *testing.T) {
	meta := Meta{
		Pos: []PosSpec{{Name: "METHOD"}, {Name: "PATH"}},
		Attrs: []AttrSpec{
			{Key: "name"},
			{Key: "schedule"},
		},
	}

	tests := []struct {
		name     string
		args     string
		wantPos  []string
		wantAttr map[string]string
		wantErrs int
	}{
		{"plain", "GET /login", []string{"GET", "/login"}, map[string]string{}, 0},
		{"with option", "GET /login name=auth", []string{"GET", "/login"}, map[string]string{"name": "auth"}, 0},
		{"quoted value", `GET /login name="two words"`, []string{"GET", "/login"}, map[string]string{"name": "two words"}, 0},
		{"unknown key", "GET /login nope=x", []string{"GET", "/login"}, map[string]string{}, 1},
		{"duplicate key", "GET /login name=a name=b", []string{"GET", "/login"}, map[string]string{"name": "a"}, 1},
		{"missing positional", "GET", []string{"GET"}, map[string]string{}, 1},
		{"missing both", "", nil, map[string]string{}, 1},
		{"extra positional", "GET /login /x", []string{"GET", "/login"}, map[string]string{}, 1},
		{"positional after option", "GET name=a /login", []string{"GET"}, map[string]string{"name": "a"}, 2},
		{"equals in positional", "GET /q?a=b", []string{"GET", "/q?a=b"}, map[string]string{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ds := ParseArgs(ann(tt.args), meta)
			var pos []string
			for _, p := range got.Pos {
				pos = append(pos, p.Text)
			}
			if !reflect.DeepEqual(pos, tt.wantPos) {
				t.Errorf("positionals = %v, want %v", pos, tt.wantPos)
			}
			attrs := map[string]string{}
			for k, v := range got.Attr {
				attrs[k] = v.Text
			}
			if !reflect.DeepEqual(attrs, tt.wantAttr) {
				t.Errorf("attrs = %v, want %v", attrs, tt.wantAttr)
			}
			errs, _ := ds.Counts()
			if errs != tt.wantErrs {
				t.Errorf("errors = %d, want %d (%v)", errs, tt.wantErrs, ds)
			}
		})
	}
}

func TestParseArgsRequired(t *testing.T) {
	meta := Meta{Attrs: []AttrSpec{{Key: "name", Required: true}}}
	_, ds := ParseArgs(ann(""), meta)
	if errs, _ := ds.Counts(); errs != 1 {
		t.Errorf("missing required option: errors = %d, want 1 (%v)", errs, ds)
	}
	_, ds = ParseArgs(ann("name=x"), meta)
	if errs, _ := ds.Counts(); errs != 0 {
		t.Errorf("provided required option: errors = %d, want 0 (%v)", errs, ds)
	}
}

func TestArgPositions(t *testing.T) {
	meta := Meta{Pos: []PosSpec{{Name: "METHOD"}, {Name: "PATH"}}}
	a := ann("GET /login")
	got, _ := ParseArgs(a, meta)
	if col := a.ArgPos(got.Pos[1].Col).Column; col != 15+4 {
		t.Errorf("PATH column = %d, want %d", col, 19)
	}
}
