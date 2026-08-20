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

func testKnownModules() map[string]bool {
	known := map[string]bool{}
	for _, m := range []string{
		"forms", "cache", "cli", "cli/directive", "ratelimit", "mail",
		"httpserver", "storage", "validation", "assetmapper",
		"assetmapper/directive", "gen", "diag", "config/directive",
		"jobs/directive", "migrations", "migrations/directive",
		"router/directive", "templates", "templates/directive",
		"web/directive",
	} {
		known["github.com/gofabrik/fabrik/"+m] = true
	}
	return known
}

func TestClassifyTidy(t *testing.T) {
	known := testKnownModules()
	const intraErr = "go: downloading github.com/gofabrik/fabrik/validation v0.1.0\n" +
		"go: github.com/gofabrik/fabrik/forms imports\n" +
		"\tgithub.com/gofabrik/fabrik/validation: reading github.com/gofabrik/fabrik/validation/go.mod at revision validation/v0.1.0: unknown revision validation/v0.1.0\n"
	if got := ClassifyTidy(nil, nil, 0, known); got != StatusClean {
		t.Errorf("exit 0 = %q, want clean", got)
	}
	if got := ClassifyTidy([]byte("--- a/go.mod\n+++ b/go.mod\n"), nil, 1, known); got != StatusFindings {
		t.Errorf("drift (diff, exit 1) = %q, want findings", got)
	}
	if got := ClassifyTidy(nil, []byte("go: some error\n"), 1, known); got != StatusError {
		t.Errorf("error (no diff, exit 1) = %q, want error", got)
	}
	// The expected unpublished revision failure makes standalone tidy unchecked.
	if got := ClassifyTidy(nil, []byte(intraErr), 1, known); got != StatusUnchecked {
		t.Errorf("intra-repo tidy failure = %q, want unchecked", got)
	}
	// Real drift outranks a trailing intra-repo resolution failure.
	if got := ClassifyTidy([]byte("--- a/go.mod\n+++ b/go.mod\n"), []byte(intraErr), 1, known); got != StatusFindings {
		t.Errorf("drift plus intra-repo failure = %q, want findings", got)
	}
	// A wrong intra-repo version pin is a real error, not the expected v0.1.0 failure.
	wrongVersion := strings.ReplaceAll(intraErr, "v0.1.0", "v0.2.0")
	if got := ClassifyTidy(nil, []byte(wrongVersion), 1, known); got != StatusError {
		t.Errorf("wrong intra-repo version = %q, want error", got)
	}
	// An unrelated resolution failure alongside the intra-repo one is still an error,
	// including one that carries no "unknown revision" phrase.
	mixed := intraErr + "go: example.com/other@latest: no matching versions for query \"latest\"\n"
	if got := ClassifyTidy(nil, []byte(mixed), 1, known); got != StatusError {
		t.Errorf("mixed intra-repo and unrelated failure = %q, want error", got)
	}
	// Nested intra-repo modules produce multi-segment revisions and are still benign.
	nested := "go: github.com/gofabrik/fabrik/fabrik imports\n" +
		"\tgithub.com/gofabrik/fabrik/cli/directive: reading github.com/gofabrik/fabrik/cli/directive/go.mod at revision cli/directive/v0.1.0: unknown revision cli/directive/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(nested), 1, known); got != StatusUnchecked {
		t.Errorf("nested intra-repo tidy failure = %q, want unchecked", got)
	}
	mismatch := "\tgithub.com/gofabrik/fabrik/forms: reading github.com/gofabrik/fabrik/forms/go.mod at revision validation/v0.1.0: unknown revision validation/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(mismatch), 1, known); got != StatusError {
		t.Errorf("mismatched reason components = %q, want error", got)
	}

	siblingRequirer := "\tgithub.com/gofabrik/fabrik/forms: reading github.com/gofabrik/fabrik/validation/go.mod at revision validation/v0.1.0: unknown revision validation/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(siblingRequirer), 1, known); got != StatusUnchecked {
		t.Errorf("sibling requirer with matching reason = %q, want unchecked", got)
	}

	cacheDbtest := "go: downloading github.com/gofabrik/fabrik/cache v0.1.0\n" +
		"go: github.com/gofabrik/fabrik/cache/dbtest: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(cacheDbtest), 1, known); got != StatusUnchecked {
		t.Errorf("cache/dbtest sibling = %q, want unchecked", got)
	}

	cliDirective := "go: downloading github.com/gofabrik/fabrik/cli v0.1.0\n" +
		"go: github.com/gofabrik/fabrik/cli/directive: reading github.com/gofabrik/fabrik/cli/go.mod at revision cli/v0.1.0: unknown revision cli/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(cliDirective), 1, known); got != StatusUnchecked {
		t.Errorf("cli/directive sibling = %q, want unchecked", got)
	}

	ratelimitDbtest := "go: downloading github.com/gofabrik/fabrik/ratelimit v0.1.0\n" +
		"go: github.com/gofabrik/fabrik/ratelimit/dbtest: reading github.com/gofabrik/fabrik/ratelimit/go.mod at revision ratelimit/v0.1.0: unknown revision ratelimit/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(ratelimitDbtest), 1, known); got != StatusUnchecked {
		t.Errorf("ratelimit/dbtest sibling = %q, want unchecked", got)
	}

	// Import and tested-by chains may end in intra-repo resolution errors.
	examplesDemo := "go: downloading github.com/gofabrik/fabrik/cache v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/assetmapper v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/flash v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/forms v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/jobs v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/query v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/ratelimit v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/router v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/session v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/storage v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/templates v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/validation v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/web v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/cli v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/config v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/httpserver v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/migrations v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/mail v0.1.0\n" +
		"go: downloading github.com/mattn/go-isatty v0.0.20\n" +
		"go: downloading github.com/ncruces/go-strftime v1.0.0\n" +
		"go: downloading golang.org/x/tools v0.49.0\n" +
		"go: demo imports\n" +
		"\tgithub.com/gofabrik/fabrik/cli: reading github.com/gofabrik/fabrik/cli/go.mod at revision cli/v0.1.0: unknown revision cli/v0.1.0\n" +
		"go: demo imports\n" +
		"\tgithub.com/gofabrik/fabrik/httpserver: reading github.com/gofabrik/fabrik/httpserver/go.mod at revision httpserver/v0.1.0: unknown revision httpserver/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tgithub.com/gofabrik/fabrik/cache: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tgithub.com/gofabrik/fabrik/mail: reading github.com/gofabrik/fabrik/mail/go.mod at revision mail/v0.1.0: unknown revision mail/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tgithub.com/gofabrik/fabrik/mail/templates: reading github.com/gofabrik/fabrik/mail/go.mod at revision mail/v0.1.0: unknown revision mail/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tgithub.com/gofabrik/fabrik/ratelimit: reading github.com/gofabrik/fabrik/ratelimit/go.mod at revision ratelimit/v0.1.0: unknown revision ratelimit/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tgithub.com/gofabrik/fabrik/storage: reading github.com/gofabrik/fabrik/storage/go.mod at revision storage/v0.1.0: unknown revision storage/v0.1.0\n" +
		"go: demo/web imports\n" +
		"\tgithub.com/gofabrik/fabrik/forms: reading github.com/gofabrik/fabrik/forms/go.mod at revision forms/v0.1.0: unknown revision forms/v0.1.0\n" +
		"go: demo/web imports\n" +
		"\tgithub.com/gofabrik/fabrik/validation: reading github.com/gofabrik/fabrik/validation/go.mod at revision validation/v0.1.0: unknown revision validation/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tmodernc.org/sqlite tested by\n" +
		"\tmodernc.org/sqlite.test imports\n" +
		"\tgithub.com/google/pprof/profile: github.com/gofabrik/fabrik/cache@v0.1.0: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tmodernc.org/sqlite tested by\n" +
		"\tmodernc.org/sqlite.test imports\n" +
		"\tmodernc.org/fileutil/ccgo: github.com/gofabrik/fabrik/cache@v0.1.0: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n" +
		"go: demo imports\n" +
		"\tgithub.com/gofabrik/fabrik/config imports\n" +
		"\tgopkg.in/yaml.v3 tested by\n" +
		"\tgopkg.in/yaml.v3.test imports\n" +
		"\tgopkg.in/check.v1: github.com/gofabrik/fabrik/cache@v0.1.0: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tmodernc.org/sqlite imports\n" +
		"\tmodernc.org/libc tested by\n" +
		"\tmodernc.org/libc.test imports\n" +
		"\tmodernc.org/cc/v4: github.com/gofabrik/fabrik/cache@v0.1.0: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tmodernc.org/sqlite imports\n" +
		"\tmodernc.org/libc tested by\n" +
		"\tmodernc.org/libc.test imports\n" +
		"\tmodernc.org/ccgo/v4/lib: github.com/gofabrik/fabrik/cache@v0.1.0: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tmodernc.org/sqlite imports\n" +
		"\tmodernc.org/libc tested by\n" +
		"\tmodernc.org/libc.test imports\n" +
		"\tmodernc.org/goabi0: github.com/gofabrik/fabrik/cache@v0.1.0: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tmodernc.org/sqlite imports\n" +
		"\tmodernc.org/libc tested by\n" +
		"\tmodernc.org/libc.test imports\n" +
		"\tgolang.org/x/tools/go/packages imports\n" +
		"\tgolang.org/x/sync/errgroup: github.com/gofabrik/fabrik/cache@v0.1.0: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n" +
		"go: demo/shared imports\n" +
		"\tmodernc.org/sqlite imports\n" +
		"\tmodernc.org/libc tested by\n" +
		"\tmodernc.org/libc.test imports\n" +
		"\tgolang.org/x/tools/go/packages imports\n" +
		"\tgolang.org/x/tools/internal/gocommand imports\n" +
		"\tgolang.org/x/mod/semver: github.com/gofabrik/fabrik/cache@v0.1.0: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(examplesDemo), 1, known); got != StatusUnchecked {
		t.Errorf("examples/demo transitive chains = %q, want unchecked", got)
	}

	fabrikStderr := "go: downloading github.com/gofabrik/fabrik/diag v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/config/directive v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/assetmapper/directive v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/gen v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/cli/directive v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/assetmapper v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/jobs/directive v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/migrations/directive v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/migrations v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/router/directive v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/templates/directive v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/web/directive v0.1.0\n" +
		"go: downloading github.com/gofabrik/fabrik/templates v0.1.0\n" +
		"go: github.com/gofabrik/fabrik/fabrik/internal/engine imports\n" +
		"\tgithub.com/gofabrik/fabrik/cli/directive: reading github.com/gofabrik/fabrik/cli/directive/go.mod at revision cli/directive/v0.1.0: unknown revision cli/directive/v0.1.0\n" +
		"go: github.com/gofabrik/fabrik/fabrik/internal/genconfig imports\n" +
		"\tgopkg.in/yaml.v3 tested by\n" +
		"\tgopkg.in/yaml.v3.test imports\n" +
		"\tgopkg.in/check.v1: github.com/gofabrik/fabrik/cli/directive@v0.1.0: reading github.com/gofabrik/fabrik/cli/directive/go.mod at revision cli/directive/v0.1.0: unknown revision cli/directive/v0.1.0\n" +
		"go: github.com/gofabrik/fabrik/fabrik/internal/load imports\n" +
		"\tgolang.org/x/tools/go/packages tested by\n" +
		"\tgolang.org/x/tools/go/packages.test imports\n" +
		"\tgithub.com/google/go-cmp/cmp: github.com/gofabrik/fabrik/cli/directive@v0.1.0: reading github.com/gofabrik/fabrik/cli/directive/go.mod at revision cli/directive/v0.1.0: unknown revision cli/directive/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(fabrikStderr), 1, known); got != StatusUnchecked {
		t.Errorf("fabrik module transitive = %q, want unchecked", got)
	}

	mixedForeign := cacheDbtest +
		"go: example.com/other@latest: no matching versions for query \"latest\"\n"
	if got := ClassifyTidy(nil, []byte(mixedForeign), 1, known); got != StatusError {
		t.Errorf("mixed benign + foreign failure = %q, want error", got)
	}

	// Progress lines alone do not make a failed tidy benign.
	progressOnly := "go: downloading github.com/gofabrik/fabrik/cache v0.1.0\n" +
		"go: finding github.com/gofabrik/fabrik/cache v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(progressOnly), 1, known); got != StatusError {
		t.Errorf("progress-only with exit 1 and no terminal = %q, want error", got)
	}

	terminal := "go: github.com/gofabrik/fabrik/cache/dbtest: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n"
	// Progress lines may accompany a benign terminal.
	for _, progress := range []string{
		"go: downloading github.com/gofabrik/fabrik/cache v0.1.0\n",
		"go: finding github.com/gofabrik/fabrik/cache v0.1.0\n",
	} {
		if got := ClassifyTidy(nil, []byte(progress+terminal), 1, known); got != StatusUnchecked {
			t.Errorf("progress form %q beside a benign terminal = %q, want unchecked", strings.Fields(progress)[1], got)
		}
	}

	// Resolution text embedded in an unrelated error is not benign.
	forged := "panic: unexpected state: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n"
	// Unknown workspace modules are not benign.
	stale := "go: app: reading github.com/gofabrik/fabrik/ghostmod/go.mod at revision ghostmod/v0.1.0: unknown revision ghostmod/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(stale), 1, known); got != StatusError {
		t.Errorf("unknown-module reason = %q, want error", got)
	}

	if got := ClassifyTidy(nil, []byte(forged+terminal), 1, known); got != StatusError {
		t.Errorf("forged prefix before a valid tail = %q, want error", got)
	}
	hopMismatch := "go: pkg: github.com/gofabrik/fabrik/mail@v0.1.0: reading github.com/gofabrik/fabrik/cache/go.mod at revision cache/v0.1.0: unknown revision cache/v0.1.0\n"
	if got := ClassifyTidy(nil, []byte(hopMismatch), 1, known); got != StatusError {
		t.Errorf("module hop disagreeing with the reason = %q, want error", got)
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

// Decode the full stream with v1's last-wins semantics for duplicate members.
func TestFreshnessStreamDecodedToCompletion(t *testing.T) {
	const data = `{"Path":"a","Version":"v1","Version":"v2","Update":{"Version":"v3"}}
{"Path":"b","Version":"v1"}
`
	got, err := Freshness([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "a" || got[0].From != "v2" || got[0].To != "v3" {
		t.Fatalf("got %+v, want the duplicate Version resolved last-wins", got)
	}
}

func TestFreshnessStreamMalformedTrailingErrors(t *testing.T) {
	const data = `{"Path":"a","Version":"v1"}
{"Path":`
	if _, err := Freshness([]byte(data)); err == nil {
		t.Fatal("want an error for a malformed trailing value")
	}
}

func TestFreshnessStreamEOFAfterWhitespaceIsClean(t *testing.T) {
	const data = "{\"Path\":\"a\",\"Version\":\"v1\"}\n\n  \t\n"
	if _, err := Freshness([]byte(data)); err != nil {
		t.Fatalf("trailing whitespace should decode cleanly: %v", err)
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
		"./diag":             "diag",
		"./assets/directive": "assets-directive",
		"./internal/tools":   "internal-tools",
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
	out := Render(results, StatusFindings, updates, Meta{Toolchain: "go1.27.0"}, 1<<20)

	for _, want := range []string{
		"go1.27.0", "./diag", "./gone", "findings", "not run", "error",
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
	for range 200 {
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
