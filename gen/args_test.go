package gen

import (
	"go/token"
	"reflect"
	"strings"
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

func TestLexArgs(t *testing.T) {
	type tok struct {
		text string
		col  int
	}
	tests := []struct {
		name string
		in   string
		want []tok
		open int // offset of the quote left open, or -1
	}{
		{"plain", "GET /login", []tok{{"GET", 0}, {"/login", 4}}, -1},
		{"whitespace run", "a \t b", []tok{{"a", 0}, {"b", 4}}, -1},
		{"leading and trailing space", "  a  ", []tok{{"a", 2}}, -1},
		{"quoted value", `name="two words"`, []tok{{"name=two words", 0}}, -1},
		{"padding kept inside quotes", `x=" spaced "`, []tok{{"x= spaced ", 0}}, -1},
		{"leading quote", `"GET" /x`, []tok{{"GET", 0}, {"/x", 6}}, -1},
		{"mid-token quotes", `a"b"c d`, []tok{{"abc", 0}, {"d", 6}}, -1},
		{"empty quoted token", `a "" b`, []tok{{"a", 0}, {"", 2}, {"b", 5}}, -1},
		{"escaped quote", `label="say \"hi\""`, []tok{{`label=say "hi"`, 0}}, -1},
		{"escaped backslash", `p="a\\b"`, []tok{{`p=a\b`, 0}}, -1},
		{"backslash outside quotes", `a\b`, []tok{{`a\b`, 0}}, -1},
		{"backslash before plain byte", `"a\b"`, []tok{{`a\b`, 0}}, -1},
		{"unterminated", `name="unterminated rest here`, []tok{{"name=unterminated rest here", 0}}, 5},
		{"quote after backslash outside quotes", `a\"`, []tok{{`a\`, 0}}, 2},
		{"trailing backslash inside quotes", `x="a\`, []tok{{`x=a\`, 0}}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, open := lexArgs(tt.in)
			var got []tok
			for _, a := range args {
				got = append(got, tok{a.Text, a.Col})
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("tokens = %v, want %v", got, tt.want)
			}
			if open != tt.open {
				t.Errorf("open quote = %d, want %d", open, tt.open)
			}
			for _, a := range args {
				if len(a.srcCols) != len(a.Text) {
					t.Errorf("token %q: %d source offsets for %d bytes", a.Text, len(a.srcCols), len(a.Text))
				}
			}
		})
	}
}

func TestOptionValueColumn(t *testing.T) {
	meta := Meta{Pos: []PosSpec{{Name: "P"}}, Attrs: []AttrSpec{{Key: "name"}}}
	tests := []struct {
		name string
		args string
		want int
	}{
		{"bare value", "name=x", 5},
		{"quoted value", `name="x y"`, 6},
		{"quoted key", `"name"=x`, 7},
		{"quoted key and value", `"name"="x"`, 8},
		{"empty value", "name=", 5},
		{"empty quoted value", `name=""`, 5},
		{"escaped quote in value", `name="a\"b"`, 6},
		{"after a positional", "/x name=y", 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := ParseArgs(ann(tt.args), meta)
			val, ok := got.Attr["name"]
			if !ok {
				t.Fatalf("name= not parsed as an option")
			}
			if val.Col != tt.want {
				t.Errorf("value column = %d, want %d", val.Col, tt.want)
			}
		})
	}
}

func TestUnterminatedQuoteDiagnostic(t *testing.T) {
	meta := Meta{Pos: []PosSpec{{Name: "P"}}, Attrs: []AttrSpec{{Key: "name"}}}
	a := ann(`GET name="oops`)
	_, ds := ParseArgs(a, meta)
	if len(ds) != 1 {
		t.Fatalf("diagnostics = %v, want one", ds)
	}
	if ds[0].Message != "unterminated quote" {
		t.Errorf("message = %q, want %q", ds[0].Message, "unterminated quote")
	}
	if got, want := ds[0].Pos.Column, a.ArgPos(9).Column; got != want {
		t.Errorf("column = %d, want %d", got, want)
	}
	if ds[0].Help == "" {
		t.Error("unterminated quote reported without help")
	}
}

func TestInvalidOptionKeyDiagnostic(t *testing.T) {
	meta := Meta{Pos: []PosSpec{{Name: "P"}}, Attrs: []AttrSpec{{Key: "name"}}}
	for _, key := range []string{"123", "1name"} {
		a := ann("/x " + key + "=y")
		_, ds := ParseArgs(a, meta)
		if len(ds) != 1 {
			t.Fatalf("%s: diagnostics = %v, want one", key, ds)
		}
		if !strings.Contains(ds[0].Message, "invalid option key") {
			t.Errorf("%s: message = %q, want it to report an invalid option key", key, ds[0].Message)
		}
		if got, want := ds[0].Pos.Column, a.ArgPos(3).Column; got != want {
			t.Errorf("%s: column = %d, want %d", key, got, want)
		}
		if ds[0].Help != knownOptionsHelp(meta) {
			t.Errorf("%s: help = %q, want the known-options help", key, ds[0].Help)
		}
	}
}

func TestParseArgs(t *testing.T) {
	meta := Meta{
		Pos: []PosSpec{{Name: "METHOD"}, {Name: "PATH"}},
		Attrs: []AttrSpec{
			{Key: "name"},
			{Key: "schedule"},
			{Key: "_debug"},
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
		{"escaped quote in value", `GET /login name="say \"hi\""`, []string{"GET", "/login"}, map[string]string{"name": `say "hi"`}, 0},
		{"empty quoted value", `GET /login name=""`, []string{"GET", "/login"}, map[string]string{"name": ""}, 0},
		{"quoted key", `GET /login "name"=auth`, []string{"GET", "/login"}, map[string]string{"name": "auth"}, 0},
		{"underscore key", "GET /login _debug=1", []string{"GET", "/login"}, map[string]string{"_debug": "1"}, 0},
		{"digit-led key", "GET /login 123=x", []string{"GET", "/login"}, map[string]string{}, 1},
		{"digit-led mixed key", "GET /login 1name=x", []string{"GET", "/login"}, map[string]string{}, 1},
		{"underscore-led key accepted", "GET /login _x=1", []string{"GET", "/login"}, map[string]string{}, 1},
		{"digit-led key opens options", "GET 123=x /login", []string{"GET"}, map[string]string{}, 3},
		{"unterminated quote", `GET /login name="oops`, []string{"GET", "/login"}, map[string]string{"name": "oops"}, 1},
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

	a = ann(`"GET" /login`)
	got, _ = ParseArgs(a, meta)
	if col := got.Pos[0].Col; col != 0 {
		t.Errorf("quoted METHOD column = %d, want 0 (the opening quote)", col)
	}
	if col := got.Pos[1].Col; col != 6 {
		t.Errorf("PATH column after a quoted METHOD = %d, want 6", col)
	}
}
