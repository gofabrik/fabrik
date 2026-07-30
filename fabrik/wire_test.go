package main

import (
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/gen"
)

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
	dir, check, opts, err := parseWireArgs([]string{"-check", "-comments", "full", "app"})
	if err != nil {
		t.Fatal(err)
	}
	if dir != "app" || !check || opts.Comments != gen.CommentsFull {
		t.Fatalf("parseWireArgs = %q, %v, %+v", dir, check, opts)
	}
	dir, check, opts, err = parseWireArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if dir != "." || check || opts.Comments != gen.CommentsSections {
		t.Fatalf("defaults = %q, %v, %+v", dir, check, opts)
	}
}
