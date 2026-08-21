package storage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/gofabrik/fabrik/storage"
	"io"
)

func TestLocalListScopedIgnoresUnreadableSibling(t *testing.T) {
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
	for _, k := range []string{"a/1", "a/2", "b/1"} {
		if err := s.Put(ctx, k, strings.NewReader(k)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(dir, "b"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(filepath.Join(dir, "b"), 0o700); err != nil { // #nosec G302 -- restoring test directory permissions
			t.Errorf("restore directory permissions: %v", err)
		}
	})

	var keys []string
	for info, err := range s.List(ctx, "a/") {
		if err != nil {
			t.Fatalf("List(a/) hit unreadable sibling: %v", err)
		}
		keys = append(keys, info.Key)
	}
	want := []string{"a/1", "a/2"}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] {
		t.Fatalf("List(a/) = %v, want %v", keys, want)
	}
}

func TestLocalPutTightensPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions only")
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
	if err := s.Put(ctx, "users/1/blob", strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"users/1", ".tmp", "users/1/blob"} {
		fi, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s perm = %04o, want group/other bits zero", rel, fi.Mode().Perm())
		}
	}
}

func TestLocalListInputSpace(t *testing.T) {
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
	for _, k := range []string{
		"users/1234/a", "users/1234/b", "users/9/x",
		"other/big",
		"a.txt", "a/b",
	} {
		if err := s.Put(ctx, k, strings.NewReader(k)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Put(ctx, "users/1234/blob", strings.NewReader("b")); err != nil {
		t.Fatal(err)
	}

	listKeys := func(prefix string) ([]string, error) {
		var keys []string
		for info, err := range s.List(ctx, prefix) {
			if err != nil {
				return nil, err
			}
			keys = append(keys, info.Key)
		}
		return keys, nil
	}

	t.Run("empty", func(t *testing.T) {
		keys, err := listKeys("")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"a.txt", "a/b", "other/big", "users/1234/a", "users/1234/b", "users/1234/blob", "users/9/x"}
		if len(keys) != len(want) {
			t.Fatalf("List(\"\") = %v, want %v", keys, want)
		}
		for i, k := range want {
			if keys[i] != k {
				t.Fatalf("List(\"\")[%d] = %q, want %q", i, keys[i], k)
			}
		}
	})

	t.Run("bare_segment", func(t *testing.T) {
		keys, err := listKeys("users")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"users/1234/a", "users/1234/b", "users/1234/blob", "users/9/x"}
		if !slices.Equal(keys, want) {
			t.Fatalf("List(\"users\") = %v, want %v", keys, want)
		}
	})

	t.Run("partial_segment", func(t *testing.T) {
		keys, err := listKeys("users/12")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"users/1234/a", "users/1234/b", "users/1234/blob"}
		if !slices.Equal(keys, want) {
			t.Fatalf("List(\"users/12\") = %v, want %v", keys, want)
		}
	})

	t.Run("trailing_slash", func(t *testing.T) {
		keys, err := listKeys("users/1234/")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"users/1234/a", "users/1234/b", "users/1234/blob"}
		if !slices.Equal(keys, want) {
			t.Fatalf("List(\"users/1234/\") = %v, want %v", keys, want)
		}
	})

	t.Run("nested", func(t *testing.T) {
		keys, err := listKeys("users/1234/a")
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 1 || keys[0] != "users/1234/a" {
			t.Fatalf("List(\"users/1234/a\") = %v, want [users/1234/a]", keys)
		}
	})

	t.Run("missing_directory", func(t *testing.T) {
		keys, err := listKeys("nope/")
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 0 {
			t.Fatalf("List(\"nope/\") = %v, want empty", keys)
		}
	})

	t.Run("enotdir", func(t *testing.T) {
		keys, err := listKeys("users/1234/blob/sub/")
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 0 {
			t.Fatalf("List(\"users/1234/blob/sub/\") = %v, want empty", keys)
		}
	})

	t.Run("lexical_order", func(t *testing.T) {
		// WalkDir order differs from lexical key order for "a.txt" and "a/b".
		keys, err := listKeys("a")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"a.txt", "a/b"}
		if !slices.Equal(keys, want) {
			t.Fatalf("List(\"a\") = %v, want %v", keys, want)
		}
	})
}

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
		if err := os.Chmod(filepath.Join(dir, "sub"), 0o755); err != nil { // #nosec G302 -- restoring test directory permissions
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

type pruningReader struct {
	data  io.Reader
	prune string
	once  bool
}

func (r *pruningReader) Read(p []byte) (int, error) {
	if !r.once {
		r.once = true
		if err := os.RemoveAll(r.prune); err != nil {
			return 0, err
		}
	}
	return r.data.Read(p)
}

func TestLocalPutRecreatedParentPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions only")
	}
	dir := t.TempDir()
	s, err := storage.NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	r := &pruningReader{data: strings.NewReader("v"), prune: filepath.Join(dir, "users")}
	if err := s.Put(context.Background(), "users/1/blob", r); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"users", "users/1"} {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Fatalf("recreated %s perm = %o, want group/other bits zero", p, fi.Mode().Perm())
		}
	}
}
