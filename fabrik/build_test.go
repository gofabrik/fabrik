package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBuildArgs_PropagatesBuildTag(t *testing.T) {
	got := buildArgs("", "e2e", ".")
	want := []string{"build", "-tags=e2e", "."}
	if !slices.Equal(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
}

func TestBuildArgs_OmitsTagsWhenUnset(t *testing.T) {
	got := buildArgs("", "", ".")
	want := []string{"build", "."}
	if !slices.Equal(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
}

func TestBuildArgs_KeepsOutputFlag(t *testing.T) {
	got := buildArgs("bin", "e2e", ".")
	want := []string{"build", "-o", "bin", "-tags=e2e", "."}
	if !slices.Equal(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
}

func TestBuildCmdRefusesEmbeddedOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module app\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fabrik.yaml"), []byte("generate:\n  emit: embedded\n  dir: appwire\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := buildCmd([]string{dir}); err == nil || !strings.Contains(err.Error(), "embedded") {
		t.Fatalf("err = %v, want embedded refusal", err)
	}
}
