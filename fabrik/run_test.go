package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestModuleRoot_WalksUpToGoMod(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "web", "assets")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := moduleRoot(nested)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Errorf("moduleRoot(%s) = %s, want %s", nested, got, root)
	}
	if got, err := moduleRoot(root); err != nil || got != root {
		t.Errorf("moduleRoot at the root = %s, %v", got, err)
	}
}

func TestModuleRoot_ErrorsWithoutGoMod(t *testing.T) {
	if _, err := moduleRoot(t.TempDir()); err == nil {
		t.Fatal("expected an error outside any module")
	}
}

func TestRunArgs_PropagatesBuildTag(t *testing.T) {
	got := runArgs("e2e", ".", []string{"serve"})
	want := []string{"run", "-tags=e2e", ".", "serve"}
	if !slices.Equal(got, want) {
		t.Errorf("runArgs = %v, want %v", got, want)
	}
}

func TestRunArgs_OmitsTagsWhenUnset(t *testing.T) {
	got := runArgs("", ".", nil)
	want := []string{"run", "."}
	if !slices.Equal(got, want) {
		t.Errorf("runArgs = %v, want %v", got, want)
	}
}

func TestRunEnv_DefaultsDevelopmentOnlyWhenUnset(t *testing.T) {
	t.Setenv("FABRIK_ENV", "production")
	if env := runEnv(); env != nil {
		t.Errorf("explicit FABRIK_ENV must pass through untouched, got injected env %v", env)
	}

	t.Setenv("FABRIK_ENV", "")
	if env := runEnv(); env != nil {
		t.Errorf("explicitly empty FABRIK_ENV must stay explicit, got %v", env)
	}

	os.Unsetenv("FABRIK_ENV")
	env := runEnv()
	if !slices.Contains(env, "FABRIK_ENV=development") {
		t.Errorf("unset FABRIK_ENV: injected env %v is missing FABRIK_ENV=development", env)
	}
}

func TestRunCommandRefusesEmbeddedOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module app\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fabrik.yaml"), []byte("generate:\n  emit: embedded\n  dir: appwire\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCommand(dir, nil); err == nil || !strings.Contains(err.Error(), "embedded") {
		t.Fatalf("err = %v, want embedded refusal", err)
	}
}
