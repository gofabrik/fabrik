package storage

import (
	"context"
	"strings"
	"testing"
)

func TestPruneParentsSparesBlobAtParentPath(t *testing.T) {
	s, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close local storage: %v", err)
		}
	})
	ctx := context.Background()
	if err := s.Put(ctx, "a", strings.NewReader("blob")); err != nil {
		t.Fatal(err)
	}
	// A blob installed at a parent path during pruning must survive.
	s.pruneParents("a/b")
	if _, err := s.Stat(ctx, "a"); err != nil {
		t.Fatalf("prune removed a blob at the parent path: %v", err)
	}
}
