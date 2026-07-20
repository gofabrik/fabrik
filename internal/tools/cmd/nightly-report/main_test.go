package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gofabrik/fabrik/internal/tools/nightlyreport"
)

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadFreshness(t *testing.T) {
	if s, _ := loadFreshness("", 0); s != nightlyreport.StatusNotRun {
		t.Errorf("empty path = %q, want not run", s)
	}
	if s, _ := loadFreshness(filepath.Join(t.TempDir(), "missing.json"), 0); s != nightlyreport.StatusError {
		t.Errorf("read failure = %q, want error", s)
	}
	f := filepath.Join(t.TempDir(), "fresh.json")
	if err := os.WriteFile(f, []byte(`{"Path":"golang.org/x/text","Version":"v0.36.0","Update":{"Version":"v0.38.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, ups := loadFreshness(f, 0)
	if s != nightlyreport.StatusFindings || len(ups) != 1 {
		t.Errorf("valid file = %q with %d updates, want findings/1", s, len(ups))
	}
}

func TestClassifyModuleNotRun(t *testing.T) {
	r := classifyModule(filepath.Join(t.TempDir(), "absent"), "./m")
	for _, s := range []nightlyreport.Status{r.Lint, r.Vuln, r.Test, r.Tidy} {
		if s != nightlyreport.StatusNotRun {
			t.Errorf("absent module dir: got %q, want not run", s)
		}
	}
}

func TestClassifyModuleComplete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "m")
	write(t, dir, map[string]string{
		"ran":          "ran\n",
		"lint.json":    `{"Issues":[{"FromLinter":"revive"}]}`,
		"lint.txt":     "m.go:1: something (revive)\n",
		"lint.outcome": "success",
		"vuln.json":    `{"config":{"scan_level":"symbol"}}`,
		"vuln.exit":    "0",
		"vuln.err":     "",
		"test.json":    `{"Action":"pass","Package":"p"}`,
		"test.exit":    "0",
		"test.err":     "",
		"tidy.txt":     "",
		"tidy.err":     "",
		"tidy.exit":    "0",
	})
	r := classifyModule(dir, "./m")
	if r.Lint != nightlyreport.StatusFindings {
		t.Errorf("lint = %q, want findings", r.Lint)
	}
	if r.Vuln != nightlyreport.StatusClean || r.Test != nightlyreport.StatusClean || r.Tidy != nightlyreport.StatusClean {
		t.Errorf("vuln/test/tidy = %q/%q/%q, want clean", r.Vuln, r.Test, r.Tidy)
	}
}

func TestClassifyModuleRanButOutputsMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "m")
	write(t, dir, map[string]string{"ran": "ran\n"})
	r := classifyModule(dir, "./m")
	for _, s := range []nightlyreport.Status{r.Lint, r.Vuln, r.Test, r.Tidy} {
		if s != nightlyreport.StatusError {
			t.Errorf("ran but outputs missing: got %q, want error", s)
		}
	}
}

func TestClassifyModuleLintOutcomeFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "m")
	write(t, dir, map[string]string{
		"ran":          "ran\n",
		"lint.json":    `{"Issues":[]}`,
		"lint.outcome": "failure",
	})
	if r := classifyModule(dir, "./m"); r.Lint != nightlyreport.StatusError {
		t.Errorf("lint outcome failure = %q, want error", r.Lint)
	}
}

func TestClassifyModuleMalformedExitMarker(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "m")
	write(t, dir, map[string]string{
		"ran":       "ran\n",
		"vuln.json": `{"config":{"scan_level":"symbol"}}`,
		"vuln.exit": "not-a-number",
	})
	if r := classifyModule(dir, "./m"); r.Vuln != nightlyreport.StatusError {
		t.Errorf("malformed vuln.exit = %q, want error", r.Vuln)
	}
}

func TestClassifyModuleErrorDetailUsesStderr(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "m")
	write(t, dir, map[string]string{
		"ran":       "ran\n",
		"vuln.err":  "govulncheck: loading packages failed\n",
		"vuln.exit": "1",
	})
	r := classifyModule(dir, "./m")
	if r.Vuln != nightlyreport.StatusError {
		t.Fatalf("vuln = %q, want error", r.Vuln)
	}
	if r.VulnDetail != "govulncheck: loading packages failed\n" {
		t.Errorf("error detail should be the stderr, got %q", r.VulnDetail)
	}
}
