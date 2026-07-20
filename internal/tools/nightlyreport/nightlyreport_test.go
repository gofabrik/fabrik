package nightlyreport

import (
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	if got := Resolve(false, false, StatusClean); got != StatusNotRun {
		t.Errorf("no artifact = %q, want not run", got)
	}
	if got := Resolve(true, false, StatusClean); got != StatusError {
		t.Errorf("ran, output missing = %q, want error", got)
	}
	if got := Resolve(true, true, StatusFindings); got != StatusFindings {
		t.Errorf("ran, present = %q, want the classified status", got)
	}
}

func TestClassifyLint(t *testing.T) {
	tests := []struct {
		name string
		data string
		want Status
	}{
		{"clean", `{"Issues":[]}`, StatusClean},
		{"findings", `{"Issues":[{"FromLinter":"revive"}]}`, StatusFindings},
		{"typecheck is error", `{"Issues":[{"FromLinter":"typecheck"}]}`, StatusError},
		{"typecheck among others is error", `{"Issues":[{"FromLinter":"revive"},{"FromLinter":"typecheck"}]}`, StatusError},
		{"invalid json is error", `not json`, StatusError},
		{"missing Issues envelope is error", `{}`, StatusError},
		{"empty output is error", ``, StatusError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyLint([]byte(tt.data)); got != tt.want {
				t.Errorf("ClassifyLint = %q, want %q", got, tt.want)
			}
		})
	}
}

const vulnReachable = `{"config":{"scan_level":"symbol"}}
{"finding":{"osv":"GO-2026-1","trace":[{"module":"stdlib","package":"crypto/tls","receiver":"*Conn","function":"Read"}]}}
{"finding":{"osv":"GO-2026-1","trace":[{"module":"stdlib","package":"crypto/tls"}]}}
`

const vulnUnreachable = `{"config":{"scan_level":"symbol"}}
{"finding":{"osv":"GO-2026-2","trace":[{"module":"golang.org/x/net","package":"golang.org/x/net/http2"}]}}
`

const vulnModuleScan = `{"config":{"scan_level":"module"}}
{"finding":{"osv":"GO-2026-3","trace":[{"module":"golang.org/x/net","function":"x"}]}}
`

func TestClassifyVuln(t *testing.T) {
	tests := []struct {
		name string
		data string
		exit int
		want Status
	}{
		{"go run error", `{"config":{"scan_level":"symbol"}}`, 1, StatusError},
		{"clean", `{"config":{"scan_level":"symbol"}}`, 0, StatusClean},
		{"reachable is findings", vulnReachable, 0, StatusFindings},
		{"unreachable-only is clean", vulnUnreachable, 0, StatusClean},
		{"non-symbol scan counts any finding", vulnModuleScan, 0, StatusFindings},
		{"invalid stream is error", "boom", 0, StatusError},
		{"empty output (no config) is error", "", 0, StatusError},
		{"config without a known scan level is error", `{"config":{}}`, 0, StatusError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyVuln([]byte(tt.data), tt.exit); got != tt.want {
				t.Errorf("ClassifyVuln = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyTest(t *testing.T) {
	// #nosec G101 -- test fixture, not a real credential
	const pass = `{"Action":"run","Package":"p"}
{"Action":"pass","Package":"p"}
`
	const fail = `{"Action":"run","Package":"p","Test":"TestX"}
{"Action":"fail","Package":"p","Test":"TestX"}
`
	const buildFail = `{"Action":"build-fail","ImportPath":"p [p.test]"}
{"Action":"fail","Package":"p","FailedBuild":"p [p.test]"}
`
	tests := []struct {
		name string
		data string
		exit int
		want Status
	}{
		{"pass", pass, 0, StatusClean},
		{"test failure is findings", fail, 1, StatusFindings},
		{"build failure is error", buildFail, 1, StatusError},
		{"nonzero exit with no parseable failure is error", "", 1, StatusError},
		{"invalid json is error", "boom", 1, StatusError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyTest([]byte(tt.data), tt.exit); got != tt.want {
				t.Errorf("ClassifyTest = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyTidy(t *testing.T) {
	const intraErr = "go: downloading github.com/gofabrik/fabrik/validation v0.1.0\n" +
		"go: github.com/gofabrik/fabrik/forms imports\n" +
		"\tgithub.com/gofabrik/fabrik/validation: reading github.com/gofabrik/fabrik/validation/go.mod at revision validation/v0.1.0: unknown revision validation/v0.1.0\n"
	if got := ClassifyTidy(nil, nil, 0); got != StatusClean {
		t.Errorf("exit 0 = %q, want clean", got)
	}
	if got := ClassifyTidy([]byte("--- a/go.mod\n+++ b/go.mod\n"), nil, 1); got != StatusFindings {
		t.Errorf("drift (diff, exit 1) = %q, want findings", got)
	}
	if got := ClassifyTidy(nil, []byte("go: some error\n"), 1); got != StatusError {
		t.Errorf("error (no diff, exit 1) = %q, want error", got)
	}
	// The expected unpublished revision failure makes standalone tidy unchecked.
	if got := ClassifyTidy(nil, []byte(intraErr), 1); got != StatusUnchecked {
		t.Errorf("intra-repo tidy failure = %q, want unchecked", got)
	}
	// Real drift outranks a trailing intra-repo resolution failure.
	if got := ClassifyTidy([]byte("--- a/go.mod\n+++ b/go.mod\n"), []byte(intraErr), 1); got != StatusFindings {
		t.Errorf("drift plus intra-repo failure = %q, want findings", got)
	}
	// A wrong intra-repo version pin is a real error, not the expected v0.1.0 failure.
	wrongVersion := strings.ReplaceAll(intraErr, "v0.1.0", "v0.2.0")
	if got := ClassifyTidy(nil, []byte(wrongVersion), 1); got != StatusError {
		t.Errorf("wrong intra-repo version = %q, want error", got)
	}
	// An unrelated resolution failure alongside the intra-repo one is still an error,
	// including one that carries no "unknown revision" phrase.
	mixed := intraErr + "go: example.com/other@latest: no matching versions for query \"latest\"\n"
	if got := ClassifyTidy(nil, []byte(mixed), 1); got != StatusError {
		t.Errorf("mixed intra-repo and unrelated failure = %q, want error", got)
	}
	// Nested intra-repo modules produce multi-segment revisions and are still benign.
	nested := "go: github.com/gofabrik/fabrik/fabrik imports\n" +
		"\tgithub.com/gofabrik/fabrik/cli/directive: reading github.com/gofabrik/fabrik/cli/directive/go.mod at revision cli/directive/v0.1.0: unknown revision cli/directive/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(nested), 1); got != StatusUnchecked {
		t.Errorf("nested intra-repo tidy failure = %q, want unchecked", got)
	}
	// A leaf line whose module components disagree is not a benign match.
	mismatch := "\tgithub.com/gofabrik/fabrik/forms: reading github.com/gofabrik/fabrik/validation/go.mod at revision validation/v0.1.0: unknown revision validation/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(mismatch), 1); got != StatusError {
		t.Errorf("mismatched module components = %q, want error", got)
	}
}

func TestClassifyFreshness(t *testing.T) {
	const withUpdate = `{"Path":"golang.org/x/text","Version":"v0.36.0","Update":{"Version":"v0.38.0"}}`
	if got := ClassifyFreshness([]byte(`{"Path":"m","Version":"v1"}`), 0); got != StatusClean {
		t.Errorf("no updates = %q, want clean", got)
	}
	if got := ClassifyFreshness([]byte(withUpdate), 0); got != StatusFindings {
		t.Errorf("updates = %q, want findings", got)
	}
	if got := ClassifyFreshness([]byte(withUpdate), 1); got != StatusError {
		t.Errorf("nonzero exit = %q, want error", got)
	}
	if got := ClassifyFreshness(nil, 0); got != StatusError {
		t.Errorf("empty output = %q, want error", got)
	}
	if got := ClassifyFreshness([]byte(`{}`), 0); got != StatusError {
		t.Errorf("record without a Path = %q, want error", got)
	}
}

func TestFreshness(t *testing.T) {
	const data = `{"Path":"app","Main":true}
{"Path":"golang.org/x/text","Version":"v0.36.0","Update":{"Version":"v0.38.0"}}
{"Path":"golang.org/x/text","Version":"v0.36.0","Update":{"Version":"v0.38.0"}}
{"Path":"golang.org/x/sys","Version":"v0.46.0"}
`
	got, err := Freshness([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d updates, want 1", len(got))
	}
	if got[0].Path != "golang.org/x/text" || got[0].From != "v0.36.0" || got[0].To != "v0.38.0" {
		t.Errorf("update = %+v", got[0])
	}
}

func TestVulnSummary(t *testing.T) {
	got := VulnSummary([]byte(vulnReachable))
	if !strings.Contains(got, "GO-2026-1") || !strings.Contains(got, "crypto/tls.(*Conn).Read") {
		t.Errorf("summary missing reachable finding with receiver:\n%s", got)
	}
	if strings.Contains(VulnSummary([]byte(vulnUnreachable)), "GO-2026-2") {
		t.Errorf("unreachable finding should not appear in summary")
	}
}

func TestTestOutput(t *testing.T) {
	const data = `{"Action":"output","Package":"p","Output":"--- FAIL: TestX\n"}
{"Action":"output","Package":"p","Output":"    x_test.go:9: boom\n"}
{"Action":"fail","Package":"p"}
`
	got := TestOutput([]byte(data))
	if !strings.Contains(got, "FAIL: TestX") || !strings.Contains(got, "boom") {
		t.Errorf("test output not reconstructed:\n%s", got)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"./diag":                  "diag",
		"./assetmapper/directive": "assetmapper-directive",
		"./internal/tools":        "internal-tools",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderSummaryFreshnessAndDetail(t *testing.T) {
	results := []ModuleResult{
		{Module: "./diag", Lint: StatusFindings, Vuln: StatusClean, Test: StatusError, Tidy: StatusClean, LintDetail: "diag lint output", TestDetail: "build failed here"},
		{Module: "./gone", Lint: StatusNotRun, Vuln: StatusNotRun, Test: StatusNotRun, Tidy: StatusNotRun},
	}
	updates := []Update{{Path: "golang.org/x/text", From: "v0.36.0", To: "v0.38.0"}}
	out := Render(results, StatusFindings, updates, Meta{Toolchain: "go1.26.0"}, 1<<20)

	for _, want := range []string{
		"go1.26.0", "./diag", "./gone", "findings", "not run", "error",
		"diag lint output", "build failed here", "golang.org/x/text", "v0.38.0",
		"| Dependencies |", "Dependency freshness",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n%s", want, out)
		}
	}
}

func TestRenderFreshnessUnavailable(t *testing.T) {
	out := Render(nil, StatusError, nil, Meta{}, 1<<20)
	if strings.Contains(out, "No available updates") {
		t.Errorf("error freshness should not claim no updates:\n%s", out)
	}
	if !strings.Contains(out, "unavailable") {
		t.Errorf("error freshness should say unavailable:\n%s", out)
	}
}

func TestRenderByteBudget(t *testing.T) {
	big := strings.Repeat("x", 5000)
	results := []ModuleResult{{Module: "./m", Lint: StatusFindings, LintDetail: big}}
	out := Render(results, StatusClean, nil, Meta{}, 1500)
	if len(out) > 1500 {
		t.Errorf("report %d bytes exceeds budget 1500", len(out))
	}
	if !strings.Contains(out, "runcat") {
		t.Errorf("over-budget report should note truncation:\n%s", out)
	}
	if !strings.Contains(out, "<details>") || !strings.Contains(out, "xxxx") || !strings.Contains(out, "(truncated)") {
		t.Errorf("per-block truncation should keep a truncated block, not drop it:\n%s", out)
	}
}

func TestRenderMandatoryExceedsBudget(t *testing.T) {
	var results []ModuleResult
	for i := 0; i < 200; i++ {
		results = append(results, ModuleResult{Module: "./module-with-a-longish-name", Lint: StatusFindings})
	}
	out := Render(results, StatusClean, nil, Meta{}, 500)
	if len(out) > 500 {
		t.Errorf("report %d bytes exceeds budget 500", len(out))
	}
}

func TestRenderTinyBudget(t *testing.T) {
	results := []ModuleResult{{Module: "./m", Lint: StatusFindings, LintDetail: strings.Repeat("x", 200)}}
	for _, budget := range []int{0, 5, 10, 40} {
		out := Render(results, StatusClean, nil, Meta{}, budget)
		if len(out) > budget {
			t.Errorf("budget %d: report %d bytes exceeds it", budget, len(out))
		}
	}
}
