package main

import (
	"slices"
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
