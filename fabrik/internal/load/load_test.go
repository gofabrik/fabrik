package load

import (
	"go/parser"
	"go/token"
	"testing"

	"golang.org/x/tools/go/packages"
)

func TestErrorPosition(t *testing.T) {
	tests := []struct {
		pos       string
		file      string
		line, col int
	}{
		{"web/web.go:10:5", "web/web.go", 10, 5},
		{"web/web.go:10", "web/web.go", 10, 0},
		{`C:\app\web\web.go:10:5`, `C:\app\web\web.go`, 10, 5},
		{"-", "-", 0, 0},
		{"", "", 0, 0},
	}
	for _, tt := range tests {
		got := errorPosition(packages.Error{Pos: tt.pos})
		if got.Filename != tt.file || got.Line != tt.line || got.Column != tt.col {
			t.Errorf("errorPosition(%q) = %q:%d:%d, want %q:%d:%d",
				tt.pos, got.Filename, got.Line, got.Column, tt.file, tt.line, tt.col)
		}
	}
}

// Directive lines in one doc comment share a declaration and preserve source order.
func TestScanFileMultipleDirectivesOneDecl(t *testing.T) {
	src := `package p

//fabrik:cli:command
//fabrik:cli:flag name=dry-run type=bool
//fabrik:cli:flag name=steps type=int
func Migrate() error { return nil }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	anns, ds := ScanFile(fset, f)
	if len(ds) != 0 {
		t.Fatalf("diags: %v", ds)
	}
	if len(anns) != 3 {
		t.Fatalf("got %d annotations, want 3", len(anns))
	}
	names := []string{"cli:command", "cli:flag", "cli:flag"}
	for i, a := range anns {
		if a.Decl != anns[0].Decl {
			t.Errorf("annotation %d has a different Decl", i)
		}
		if a.Name != names[i] {
			t.Errorf("annotation %d = %q, want %q (source order)", i, a.Name, names[i])
		}
	}
	if anns[1].Args != "name=dry-run type=bool" {
		t.Errorf("args not preserved: %q", anns[1].Args)
	}
}
