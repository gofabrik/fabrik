package genconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/diag"
	"github.com/gofabrik/fabrik/gen"
)

func writeModule(t *testing.T, fabrikYAML string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module app\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fabrikYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "fabrik.yaml"), []byte(fabrikYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func containsMsg(diags diag.Diagnostics, sub string) bool {
	for _, d := range diags {
		if strings.Contains(d.Message, sub) {
			return true
		}
	}
	return false
}

func TestResolveAbsentFileKeepsDefaults(t *testing.T) {
	dir := writeModule(t, "")
	opts, diags := Resolve(dir, Overrides{})
	if len(diags) != 0 {
		t.Fatalf("diags = %v, want none", diags)
	}
	want := Options{Emit: EmitStandalone, Split: SplitOff, Comments: gen.CommentsSections}
	if opts.Emit != want.Emit || opts.Split != want.Split || opts.Comments != want.Comments ||
		opts.Dir != "" || opts.Package != "" || opts.BuildTag != "" || len(opts.Entrypoints) != 0 {
		t.Fatalf("opts = %+v, want %+v", opts, want)
	}
}

func TestResolveFullFileStandalone(t *testing.T) {
	dir := writeModule(t, `generate:
  emit: standalone
  buildtag: e2e
  comments: full
`)
	opts, diags := Resolve(dir, Overrides{})
	if len(diags) != 0 {
		t.Fatalf("diags = %v, want none", diags)
	}
	if opts.Emit != EmitStandalone || opts.BuildTag != "e2e" || opts.Comments != gen.CommentsFull {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestResolvePackageDerivedFromDirBase(t *testing.T) {
	dir := writeModule(t, `generate:
  emit: embedded
  dir: internal/app
  entrypoints: ["migrate"]
`)
	opts, diags := Resolve(dir, Overrides{})
	if opts.Package != "app" {
		t.Fatalf("Package = %q, want %q", opts.Package, "app")
	}
	if !containsMsg(diags, "not supported yet") {
		t.Fatalf("diags = %v, want an embedded not-supported-yet diagnostic", diags)
	}
}

func TestResolvePackageNotDerivableDiagnoses(t *testing.T) {
	dir := writeModule(t, `generate:
  dir: 123bad
`)
	opts, diags := Resolve(dir, Overrides{})
	if opts.Package != "" {
		t.Fatalf("Package = %q, want empty", opts.Package)
	}
	if !containsMsg(diags, "not a valid Go identifier") {
		t.Fatalf("diags = %v, want a package-not-derivable diagnostic", diags)
	}
	for _, d := range diags {
		if d.Pos.Filename == "" {
			t.Errorf("diagnostic missing a fabrik.yaml position: %+v", d)
		}
	}
}

func TestResolveExplicitPackageWinsOverDerivation(t *testing.T) {
	dir := writeModule(t, `generate:
  dir: 123bad
  package: valid
`)
	opts, diags := Resolve(dir, Overrides{})
	if opts.Package != "valid" {
		t.Fatalf("Package = %q, want %q", opts.Package, "valid")
	}
	if len(diags) != 0 {
		t.Fatalf("diags = %v, want none", diags)
	}
}

func TestResolveInvalidExplicitPackageDiagnoses(t *testing.T) {
	dir := writeModule(t, `generate:
  package: "123bad"
`)
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, "not a valid Go identifier") {
		t.Fatalf("diags = %v, want an invalid-package diagnostic", diags)
	}
}

func TestResolveMalformedYAMLDiagnoses(t *testing.T) {
	dir := writeModule(t, "generate: [\n")
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, "malformed fabrik.yaml") {
		t.Fatalf("diags = %v, want a malformed-yaml diagnostic", diags)
	}
	if diags[0].Pos.Filename == "" {
		t.Errorf("malformed-yaml diagnostic missing a fabrik.yaml position: %+v", diags[0])
	}
}

func TestResolveUnknownTopLevelKeyDiagnoses(t *testing.T) {
	dir := writeModule(t, "bogus: true\n")
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, `unknown key "bogus"`) {
		t.Fatalf("diags = %v, want an unknown-key diagnostic", diags)
	}
}

func TestResolveUnknownGenerateKeyDiagnoses(t *testing.T) {
	dir := writeModule(t, `generate:
  bogus: true
`)
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, `unknown key "bogus"`) {
		t.Fatalf("diags = %v, want an unknown-key diagnostic", diags)
	}
}

func TestResolveInvalidEmitDiagnoses(t *testing.T) {
	dir := writeModule(t, `generate:
  emit: bogus
`)
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, `invalid emit "bogus"`) {
		t.Fatalf("diags = %v, want an invalid-emit diagnostic", diags)
	}
}

func TestResolveInvalidSplitDiagnoses(t *testing.T) {
	dir := writeModule(t, `generate:
  split: bogus
`)
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, `invalid split "bogus"`) {
		t.Fatalf("diags = %v, want an invalid-split diagnostic", diags)
	}
}

func TestResolveInvalidCommentsDiagnoses(t *testing.T) {
	dir := writeModule(t, `generate:
  comments: bogus
`)
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, `invalid comments level "bogus"`) {
		t.Fatalf("diags = %v, want an invalid-comments diagnostic", diags)
	}
}

func TestResolveEntrypointsOutsideEmbeddedDiagnoses(t *testing.T) {
	dir := writeModule(t, `generate:
  entrypoints: ["migrate"]
`)
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, "entrypoints requires emit: embedded") {
		t.Fatalf("diags = %v, want an entrypoints-requires-embedded diagnostic", diags)
	}
}

func TestResolveDuplicateEntrypointsDiagnoses(t *testing.T) {
	dir := writeModule(t, `generate:
  emit: embedded
  package: app
  entrypoints: ["migrate", "migrate"]
`)
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, `duplicate entrypoint "migrate"`) {
		t.Fatalf("diags = %v, want a duplicate-entrypoint diagnostic", diags)
	}
}

func TestResolveEmitEmbeddedNotSupportedYet(t *testing.T) {
	dir := writeModule(t, `generate:
  emit: embedded
  package: app
`)
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, "emit: embedded is not supported yet") {
		t.Fatalf("diags = %v, want a not-supported-yet diagnostic", diags)
	}
}

func TestResolveSplitFragmentNotSupportedYet(t *testing.T) {
	dir := writeModule(t, `generate:
  split: fragment
`)
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, "split: fragment is not supported yet") {
		t.Fatalf("diags = %v, want a not-supported-yet diagnostic", diags)
	}
}

func TestResolveFlagOverrideBeatsFileValue(t *testing.T) {
	dir := writeModule(t, `generate:
  comments: off
`)
	full := gen.CommentsFull
	opts, diags := Resolve(dir, Overrides{Comments: &full})
	if len(diags) != 0 {
		t.Fatalf("diags = %v, want none", diags)
	}
	if opts.Comments != gen.CommentsFull {
		t.Fatalf("Comments = %v, want the override to win", opts.Comments)
	}
}

func TestResolveNoOverrideKeepsFileValue(t *testing.T) {
	dir := writeModule(t, `generate:
  comments: off
`)
	opts, _ := Resolve(dir, Overrides{})
	if opts.Comments != gen.CommentsOff {
		t.Fatalf("Comments = %v, want the file value", opts.Comments)
	}
}

func TestResolveAbsentGoModKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	opts, diags := Resolve(dir, Overrides{})
	if len(diags) != 0 {
		t.Fatalf("diags = %v, want none", diags)
	}
	if opts.Emit != EmitStandalone {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestParseCommentLevelMatchesFlagValues(t *testing.T) {
	cases := []struct {
		in   string
		want gen.CommentLevel
	}{
		{"off", gen.CommentsOff},
		{"sections", gen.CommentsSections},
		{"full", gen.CommentsFull},
	}
	for _, tc := range cases {
		got, err := ParseCommentLevel(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseCommentLevel(%q) = %v, %v", tc.in, got, err)
		}
	}
	if _, err := ParseCommentLevel("bogus"); err == nil {
		t.Error("ParseCommentLevel(bogus) = nil error, want an error")
	}
}

func TestResolveRejectsNonScalarValues(t *testing.T) {
	for field, doc := range map[string]string{
		"emit":     "generate:\n  emit: []\n",
		"split":    "generate:\n  split: {}\n",
		"dir":      "generate:\n  dir: [a]\n",
		"buildtag": "generate:\n  buildtag: {x: y}\n",
		"comments": "generate:\n  comments: [off]\n",
		"package":  "generate:\n  package: [p]\n",
	} {
		dir := writeModule(t, doc)
		_, diags := Resolve(dir, Overrides{})
		if !diags.HasFatal() || !strings.Contains(diags[0].Message, field) {
			t.Fatalf("%s: diags = %v, want scalar rejection", field, diags)
		}
	}
	dir := writeModule(t, "generate:\n  emit: embedded\n  entrypoints:\n    - [db]\n")
	_, diags := Resolve(dir, Overrides{})
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "entrypoints entries must be command paths") {
			found = true
		}
	}
	if !found {
		t.Fatalf("non-scalar entrypoint accepted: %v", diags)
	}
}

func TestResolveRejectsNullAndDuplicateKeys(t *testing.T) {
	for name, doc := range map[string]string{
		"null emit":     "generate:\n  emit: null\n",
		"null split":    "generate:\n  split:\n",
		"null comments": "generate:\n  comments: ~\n",
		"empty emit":    "generate:\n  emit: \"\"\n",
		"empty dir":     "generate:\n  dir: \"\"\n",
		"empty tag":     "generate:\n  buildtag: \"\"\n",
		"dup field":     "generate:\n  emit: standalone\n  emit: standalone\n",
		"dup generate":  "generate:\n  emit: standalone\ngenerate:\n  split: off\n",
	} {
		dir := writeModule(t, doc)
		_, diags := Resolve(dir, Overrides{})
		if !diags.HasFatal() {
			t.Fatalf("%s accepted: %v", name, diags)
		}
	}
}

func TestResolveEmptyEntrypointsOutsideEmbeddedDiagnoses(t *testing.T) {
	dir := writeModule(t, "generate:\n  entrypoints: []\n")
	_, diags := Resolve(dir, Overrides{})
	if !diags.HasFatal() || !strings.Contains(diags[0].Message, "entrypoints requires emit: embedded") {
		t.Fatalf("diags = %v, want placement rejection", diags)
	}
}

func TestResolveUnicodeBuildTagAccepted(t *testing.T) {
	dir := writeModule(t, "generate:\n  buildtag: pr\u00f3ba1\n")
	opts, diags := Resolve(dir, Overrides{})
	if diags.HasFatal() || opts.BuildTag == "" {
		t.Fatalf("unicode tag rejected: %v", diags)
	}
}

func TestResolveRejectsInvalidBuildTag(t *testing.T) {
	dir := writeModule(t, "generate:\n  buildtag: \"foo bar\"\n")
	_, diags := Resolve(dir, Overrides{})
	if !diags.HasFatal() || !strings.Contains(diags[0].Message, "invalid buildtag") {
		t.Fatalf("diags = %v, want buildtag rejection", diags)
	}
	if diags[0].Pos.Line == 0 {
		t.Fatalf("buildtag diagnostic unpositioned: %v", diags[0].Pos)
	}
}

func TestResolveMalformedYAMLCarriesLine(t *testing.T) {
	dir := writeModule(t, "generate:\n  emit: [unclosed\n")
	_, diags := Resolve(dir, Overrides{})
	if !diags.HasFatal() {
		t.Fatalf("diags = %v, want malformed yaml error", diags)
	}
	if diags[0].Pos.Line == 0 {
		t.Fatalf("malformed yaml diagnostic must carry the reported line: %v", diags[0].Pos)
	}
}

func TestEngineOptionsMapping(t *testing.T) {
	o := Options{Comments: gen.CommentsFull, BuildTag: "wired"}
	eo := o.EngineOptions()
	if eo.Comments != gen.CommentsFull || eo.BuildTag != "wired" {
		t.Fatalf("EngineOptions() = %+v", eo)
	}
}
