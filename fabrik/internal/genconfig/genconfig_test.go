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
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module app\n\ngo 1.27\n"), 0o600); err != nil {
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
	if diags.HasFatal() {
		t.Fatalf("diags = %v", diags)
	}
	if opts.Package != "app" {
		t.Fatalf("Package = %q, want %q", opts.Package, "app")
	}
	ep := opts.EntrypointPos["migrate"]
	if len(opts.Entrypoints) != 1 || !ep.IsValid() {
		t.Fatalf("entrypoints = %v with pos %v", opts.Entrypoints, opts.EntrypointPos)
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
  emit: embedded
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

func TestResolveEmitEmbeddedResolves(t *testing.T) {
	dir := writeModule(t, "generate:\n  emit: embedded\n  dir: appwire\n")
	opts, diags := Resolve(dir, Overrides{})
	if diags.HasFatal() {
		t.Fatalf("diags = %v", diags)
	}
	emitPos := opts.EmitPos
	if opts.Emit != EmitEmbedded || opts.Package != "appwire" || !emitPos.IsValid() {
		t.Fatalf("embedded not resolved: %+v", opts)
	}
}

func TestResolveEmbeddedRequiresDir(t *testing.T) {
	dir := writeModule(t, "generate:\n  emit: embedded\n")
	_, diags := Resolve(dir, Overrides{})
	if !diags.HasFatal() || !strings.Contains(diags[0].Message, "requires dir") {
		t.Fatalf("diags = %v, want dir requirement", diags)
	}
}
func TestResolveSplitFragmentResolves(t *testing.T) {
	dir := writeModule(t, "generate:\n  split: fragment\n")
	opts, diags := Resolve(dir, Overrides{})
	if diags.HasFatal() {
		t.Fatalf("diags = %v", diags)
	}
	if opts.Split != SplitFragment {
		t.Fatalf("split not resolved: %+v", opts)
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

func TestResolveDirEscapingModuleDiagnoses(t *testing.T) {
	for _, dir := range []string{"../outside", "gen/../../outside", "/abs"} {
		root := writeModule(t, "generate:\n  emit: embedded\n  dir: "+dir+"\n")
		_, diags := Resolve(root, Overrides{})
		if !containsMsg(diags, "must stay inside the module") {
			t.Fatalf("dir %q: diags = %v, want a containment diagnostic", dir, diags)
		}
	}
}

func TestResolveOverlayBeatsDisk(t *testing.T) {
	dir := writeModule(t, "generate:\n  emit: standalone\n")
	overlay := map[string][]byte{
		filepath.Join(dir, "fabrik.yaml"): []byte("generate:\n  emit: bogus\n"),
	}
	_, diags := ResolveOverlay(dir, overlay, Overrides{})
	if !containsMsg(diags, `invalid emit "bogus"`) {
		t.Fatalf("diags = %v, want the overlay content diagnosed", diags)
	}
	opts, diags := ResolveOverlay(dir, nil, Overrides{})
	if len(diags) != 0 || opts.Emit != EmitStandalone {
		t.Fatalf("opts = %+v, diags = %v, want the on-disk file without overlay", opts, diags)
	}
}

func TestResolveRejectedDirSuppressesRequiresDir(t *testing.T) {
	dir := writeModule(t, "generate:\n  emit: embedded\n  dir: ../outside\n")
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, "must stay inside the module") {
		t.Fatalf("diags = %v, want the containment diagnostic", diags)
	}
	if containsMsg(diags, "embedded requires dir") {
		t.Fatalf("diags = %v, want no cascading requires-dir diagnostic", diags)
	}
}

func TestResolveInvalidEmitSuppressesEntrypointsCheck(t *testing.T) {
	dir := writeModule(t, "generate:\n  emit: bogus\n  entrypoints:\n    - serve\n")
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, `invalid emit "bogus"`) {
		t.Fatalf("diags = %v, want the invalid-emit diagnostic", diags)
	}
	if containsMsg(diags, "entrypoints requires emit: embedded") {
		t.Fatalf("diags = %v, want no cascading entrypoints diagnostic", diags)
	}
}

func TestResolveWarnsIgnoredStandaloneFields(t *testing.T) {
	dir := writeModule(t, "generate:\n  dir: gen/app\n  package: app\n")
	_, diags := Resolve(dir, Overrides{})
	if !containsMsg(diags, "dir is ignored unless emit: embedded") ||
		!containsMsg(diags, "package is ignored unless emit: embedded") {
		t.Fatalf("diags = %v, want ignored-field warnings", diags)
	}
	if diags.HasFatal() {
		t.Fatalf("diags = %v, want warnings only", diags)
	}
}

func TestResolveMalformedEmitSuppressesEntrypointsCheck(t *testing.T) {
	for _, body := range []string{
		"generate:\n  emit:\n  entrypoints:\n    - serve\n",
		"generate:\n  emit: [a]\n  entrypoints:\n    - serve\n",
	} {
		dir := writeModule(t, body)
		_, diags := Resolve(dir, Overrides{})
		if !containsMsg(diags, "emit must be a non-empty value") {
			t.Fatalf("body %q: diags = %v, want the malformed-emit diagnostic", body, diags)
		}
		if containsMsg(diags, "entrypoints requires emit: embedded") {
			t.Fatalf("body %q: diags = %v, want no cascading entrypoints diagnostic", body, diags)
		}
	}
}
