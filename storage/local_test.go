package storage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/storage"
)

// A failure that is not absence must surface through List, never be
// swallowed as a skipped entry.
func TestLocalListYieldsNonNotExistErrors(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission-based failure needs unix non-root")
	}
	dir := t.TempDir()
	s, err := storage.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close local storage: %v", err)
		}
	})
	ctx := context.Background()
	if err := s.Put(ctx, "sub/blob", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "sub"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Errorf("restore directory permissions: %v", err)
		}
	})

	var infos, errCount int
	var sawErr error
	for _, err := range s.List(ctx, "") {
		if err != nil {
			errCount++
			sawErr = err
			continue
		}
		infos++
	}
	if errCount != 1 || sawErr == nil {
		t.Fatalf("unreadable entry: %d items, %d errors (want exactly one error)", infos, errCount)
	}
	if errors.Is(sawErr, storage.ErrNotExist) {
		t.Fatalf("permission failure surfaced as absence: %v", sawErr)
	}
}
