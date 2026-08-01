package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/fabrik/internal/genconfig"
	"github.com/gofabrik/fabrik/gen"
)

func writeGoMod(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module app\n\ngo 1.27\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCommentLevelValues(t *testing.T) {
	cases := []struct {
		in   string
		want gen.CommentLevel
	}{
		{"off", gen.CommentsOff},
		{"sections", gen.CommentsSections},
		{"full", gen.CommentsFull},
	}
	for _, tc := range cases {
		got, err := commentLevel(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("commentLevel(%q) = %v, %v", tc.in, got, err)
		}
	}
	if _, err := commentLevel("bogus"); err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Errorf("commentLevel(bogus) error = %v, want usage error naming the value", err)
	}
}

func TestWireCmdRejectsInvalidCommentLevel(t *testing.T) {
	err := wireCmd([]string{"-comments", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "invalid -comments level") {
		t.Fatalf("wireCmd error = %v, want invalid-level usage error", err)
	}
}

func TestParseWireArgsThreadsOptions(t *testing.T) {
	dir, check, ov, graph, err := parseWireArgs([]string{"-check", "-comments", "full", "app"})
	if err != nil {
		t.Fatal(err)
	}
	if dir != "app" || !check || ov.Comments == nil || *ov.Comments != gen.CommentsFull {
		t.Fatalf("parseWireArgs = %q, %v, %+v", dir, check, ov)
	}
	dir, check, ov, graph, err = parseWireArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if dir != "." || check || ov.Comments != nil || graph {
		t.Fatalf("defaults = %q, %v, %+v, %v", dir, check, ov, graph)
	}
	dir, check, ov, graph, err = parseWireArgs([]string{"--graph", "app"})
	if err != nil {
		t.Fatal(err)
	}
	if dir != "app" || check || !graph {
		t.Fatalf("parseWireArgs(--graph) = %q, %v, %v", dir, check, graph)
	}
}

func TestParseWireArgsRejectsCheckWithGraph(t *testing.T) {
	_, _, _, _, err := parseWireArgs([]string{"-check", "--graph"})
	if err == nil || !strings.Contains(err.Error(), "-check") || !strings.Contains(err.Error(), "--graph") {
		t.Fatalf("parseWireArgs error = %v, want a usage error naming both flags", err)
	}
}

func TestResolveOptionsAppliesFabrikYAML(t *testing.T) {
	dir := t.TempDir()
	writeGoMod(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "fabrik.yaml"), []byte("generate:\n  buildtag: e2e\n  comments: full\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts, resolved, err := resolveOptions(dir, genconfig.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Comments != gen.CommentsFull || opts.BuildTag != "e2e" || resolved.BuildTag != "e2e" {
		t.Fatalf("opts = %+v, resolved = %+v", opts, resolved)
	}
}

func TestResolveOptionsOverrideBeatsFile(t *testing.T) {
	dir := t.TempDir()
	writeGoMod(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "fabrik.yaml"), []byte("generate:\n  comments: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	full := gen.CommentsFull
	opts, _, err := resolveOptions(dir, genconfig.Overrides{Comments: &full})
	if err != nil {
		t.Fatal(err)
	}
	if opts.Comments != gen.CommentsFull {
		t.Fatalf("opts.Comments = %v, want the override to win", opts.Comments)
	}
}

func TestResolveOptionsReportsFatalDiagnostics(t *testing.T) {
	dir := t.TempDir()
	writeGoMod(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "fabrik.yaml"), []byte("generate:\n  emit: bogus\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := resolveOptions(dir, genconfig.Overrides{})
	if !errors.Is(err, errSilent) {
		t.Fatalf("resolveOptions error = %v, want errSilent", err)
	}
}

func TestWriteGraphSidecars(t *testing.T) {
	dir := t.TempDir()
	graph := &gen.Graph{
		Version: 1,
		Module:  "app",
		Flows:   []gen.GraphFlow{{ID: "run", Fn: "buildRun"}},
		Nodes:   []gen.GraphNode{{ID: "run/db", Flow: "run", Kind: "call", Defines: []string{"db"}, Fn: "shared.NewDB", Directive: "provider"}},
	}
	written, err := writeGraphSidecars(dir, graph)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 3 {
		t.Fatalf("written = %v", written)
	}
	for _, name := range []string{"fabrik.graph.json", "fabrik.graph.dot", "fabrik.graph.mmd"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 || data[len(data)-1] != '\n' {
			t.Errorf("%s is empty or unterminated", name)
		}
	}
	jsonData, err := os.ReadFile(filepath.Join(dir, "fabrik.graph.json"))
	if err != nil {
		t.Fatal(err)
	}
	var round gen.Graph
	if err := json.Unmarshal(jsonData, &round); err != nil {
		t.Fatalf("graph json does not round-trip: %v\n%s", err, jsonData)
	}
	if !strings.Contains(string(jsonData), "\n  \"module\": \"app\"") {
		t.Errorf("graph json is not indented:\n%s", jsonData)
	}
	if round.Nodes[0].ID != "run/db" {
		t.Errorf("round-tripped graph = %+v", round)
	}
	dot, err := os.ReadFile(filepath.Join(dir, "fabrik.graph.dot"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(dot), "digraph fabrik {") {
		t.Errorf("dot sidecar:\n%s", dot)
	}
	mmd, err := os.ReadFile(filepath.Join(dir, "fabrik.graph.mmd"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(mmd), "flowchart TB") {
		t.Errorf("mermaid sidecar:\n%s", mmd)
	}
}

func TestParseWireArgsRejectsExplicitEmptyComments(t *testing.T) {
	if _, _, _, _, err := parseWireArgs([]string{"-comments="}); err == nil {
		t.Fatal("explicit empty -comments accepted")
	}
}
