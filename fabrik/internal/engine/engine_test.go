package engine

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
	"golang.org/x/tools/txtar"
)

var update = flag.Bool("update", false, "rewrite the want/ sections of testdata fixtures")

// TestFixtures verifies golden fixtures and deterministic generation.
func TestFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/*.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no testdata fixtures: %v", err)
	}
	for _, fixture := range files {
		t.Run(strings.TrimSuffix(filepath.Base(fixture), ".txt"), func(t *testing.T) {
			t.Parallel() // fixtures share nothing; each Wire is a fresh registry
			runFixture(t, fixture)
		})
	}
}

func runFixture(t *testing.T, fixture string) {
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	ar := txtar.Parse(data)

	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	routerDir, err := filepath.Abs("../../../router")
	if err != nil {
		t.Fatal(err)
	}
	configDir, err := filepath.Abs("../../../config")
	if err != nil {
		t.Fatal(err)
	}
	var wantGen, wantDiags []byte
	hasGen, hasDiags := false, false
	for _, f := range ar.Files {
		switch f.Name {
		case "want/main.gen.go":
			wantGen, hasGen = f.Data, true
			continue
		case "want/diags":
			wantDiags, hasDiags = f.Data, true
			continue
		}
		path := filepath.Join(dir, f.Name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		// Fixtures resolve local module checkouts. No fixture source
		// imports the config package (only generated code does, and it is
		// never built here), so loading needs no go.sum and no tidy.
		data := bytes.ReplaceAll(f.Data, []byte("ROUTERDIR"), []byte(routerDir))
		data = bytes.ReplaceAll(data, []byte("CONFIGDIR"), []byte(configDir))
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Wire(dir, nil)
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	gotDiags := renderDiags(res.Diags, dir)

	if res.Src != nil {
		again, err := Wire(dir, nil)
		if err != nil {
			t.Fatalf("Wire (second run): %v", err)
		}
		if !bytes.Equal(res.Src, again.Src) {
			t.Errorf("generation is not deterministic:\nfirst:\n%s\nsecond:\n%s", res.Src, again.Src)
		}
	}

	if *update {
		updateFixture(t, fixture, ar, res.Src, gotDiags)
		return
	}

	if hasGen && !bytes.Equal(res.Src, wantGen) {
		t.Errorf("main.gen.go mismatch\n--- want ---\n%s--- got ---\n%s", wantGen, res.Src)
	}
	if !hasGen && res.Src != nil {
		t.Errorf("unexpected generated output (no want/main.gen.go in fixture):\n%s", res.Src)
	}
	if hasDiags && gotDiags != string(wantDiags) {
		t.Errorf("diagnostics mismatch\n--- want ---\n%s--- got ---\n%s", wantDiags, gotDiags)
	}
	if !hasDiags && gotDiags != "" {
		t.Errorf("unexpected diagnostics:\n%s", gotDiags)
	}
}

// renderDiags formats root-relative fixture diagnostics.
func renderDiags(ds diag.Diagnostics, root string) string {
	scrub := func(s string) string {
		return strings.ReplaceAll(s, root+string(filepath.Separator), "$WORK/")
	}
	var b strings.Builder
	for _, d := range ds {
		sev := "error"
		if d.Severity == diag.SevWarning {
			sev = "warning"
		}
		rel := strings.TrimPrefix(d.Pos.Filename, root+string(filepath.Separator))
		rel = filepath.ToSlash(rel)
		fmt.Fprintf(&b, "%s: %s:%d:%d: %s\n", sev, rel, d.Pos.Line, d.Pos.Column, scrub(d.Message))
		if d.Help != "" {
			fmt.Fprintf(&b, "  help: %s\n", scrub(d.Help))
		}
	}
	return b.String()
}

func updateFixture(t *testing.T, fixture string, ar *txtar.Archive, src []byte, diags string) {
	var kept []txtar.File
	for _, f := range ar.Files {
		if f.Name != "want/main.gen.go" && f.Name != "want/diags" {
			kept = append(kept, f)
		}
	}
	if src != nil {
		kept = append(kept, txtar.File{Name: "want/main.gen.go", Data: src})
	}
	if diags != "" {
		kept = append(kept, txtar.File{Name: "want/diags", Data: []byte(diags)})
	}
	ar.Files = kept
	if err := os.WriteFile(fixture, txtar.Format(ar), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("updated %s", fixture)
}
