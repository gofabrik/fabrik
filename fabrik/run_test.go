package main

import (
	"os"
	"path/filepath"
	"slices"
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
