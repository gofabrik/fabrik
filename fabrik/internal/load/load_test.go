package load

import (
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
