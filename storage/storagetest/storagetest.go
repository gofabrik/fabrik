// Package storagetest verifies implementations of storage.Storage:
//
//	func TestMyStorage(t *testing.T) {
//		storagetest.Run(t, func(t *testing.T) storage.Storage { ... })
//	}
package storagetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofabrik/fabrik/storage"
)

// Run exercises the Storage contract with a fresh store per subtest.
func Run(t *testing.T, factory func(t *testing.T) storage.Storage) {
	ctx := context.Background()

	t.Run("PutOpenRoundTrip", func(t *testing.T) {
		s := factory(t)
		if err := s.Put(ctx, "a/b/c.txt", strings.NewReader("hello")); err != nil {
			t.Fatal(err)
		}
		got := read(t, s, "a/b/c.txt")
		if got != "hello" {
			t.Fatalf("round trip = %q", got)
		}
	})

	t.Run("OverwriteReplacesWhole", func(t *testing.T) {
		s := factory(t)
		must(t, s.Put(ctx, "k", strings.NewReader("first version, long")))
		must(t, s.Put(ctx, "k", strings.NewReader("second")))
		if got := read(t, s, "k"); got != "second" {
			t.Fatalf("after overwrite = %q", got)
		}
	})

	t.Run("ReadersKeepTheirVersion", func(t *testing.T) {
		s := factory(t)
		must(t, s.Put(ctx, "k", strings.NewReader("old")))
		rc, err := s.Open(ctx, "k")
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		must(t, s.Put(ctx, "k", strings.NewReader("new")))
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "old" {
			t.Fatalf("reader opened before overwrite read %q, want the version it opened", b)
		}
	})

	t.Run("EmptyObjectRoundTrip", func(t *testing.T) {
		s := factory(t)
		if err := s.Put(ctx, "empty", strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
		info, err := s.Stat(ctx, "empty")
		if err != nil || info.Size != 0 {
			t.Fatalf("Stat = %+v, %v; want size 0", info, err)
		}
		if got := read(t, s, "empty"); got != "" {
			t.Fatalf("read = %q, want empty", got)
		}
	})

	t.Run("MissingIsErrNotExist", func(t *testing.T) {
		s := factory(t)
		if _, err := s.Open(ctx, "nope"); !errors.Is(err, storage.ErrNotExist) {
			t.Fatalf("Open missing: %v", err)
		}
		if _, err := s.Stat(ctx, "nope"); !errors.Is(err, storage.ErrNotExist) {
			t.Fatalf("Stat missing: %v", err)
		}
	})

	t.Run("StatFields", func(t *testing.T) {
		s := factory(t)
		must(t, s.Put(ctx, "s.bin", strings.NewReader("12345")))
		info, err := s.Stat(ctx, "s.bin")
		if err != nil {
			t.Fatal(err)
		}
		if info.Key != "s.bin" || info.Size != 5 || info.ModTime.IsZero() {
			t.Fatalf("info = %+v", info)
		}
	})

	t.Run("DeleteIsIdempotent", func(t *testing.T) {
		s := factory(t)
		must(t, s.Put(ctx, "k", strings.NewReader("x")))
		must(t, s.Delete(ctx, "k"))
		if _, err := s.Open(ctx, "k"); !errors.Is(err, storage.ErrNotExist) {
			t.Fatalf("after delete: %v", err)
		}
		must(t, s.Delete(ctx, "k"))
		must(t, s.Delete(ctx, "never-existed"))
	})

	t.Run("ListPrefixLexical", func(t *testing.T) {
		s := factory(t)
		for _, k := range []string{"b/2", "a/2", "a/10", "top"} {
			must(t, s.Put(ctx, k, strings.NewReader(k)))
		}
		var got []string
		for info, err := range s.List(ctx, "a/") {
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, info.Key)
		}
		want := []string{"a/10", "a/2"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("List(a/) = %v, want %v", got, want)
		}
		var all int
		for _, err := range s.List(ctx, "") {
			if err != nil {
				t.Fatal(err)
			}
			all++
		}
		if all != 4 {
			t.Fatalf("List(\"\") yielded %d, want 4", all)
		}
	})

	t.Run("InvalidKeysRejectedEverywhere", func(t *testing.T) {
		s := factory(t)
		for _, bad := range []string{"", "/abs", "trail/", "a//b", "../up", "a/../b", ".", ".tmp/x", "a/.hidden", "back\\slash", "control\x00key", "control\x1fkey"} {
			if err := s.Put(ctx, bad, strings.NewReader("x")); err == nil {
				t.Fatalf("Put accepted %q", bad)
			}
			if _, err := s.Open(ctx, bad); err == nil {
				t.Fatalf("Open accepted %q", bad)
			}
			if _, err := s.Stat(ctx, bad); err == nil {
				t.Fatalf("Stat accepted %q", bad)
			}
			if err := s.Delete(ctx, bad); err == nil {
				t.Fatalf("Delete accepted %q", bad)
			}
			infos, errs := drainList(s, ctx, bad+"/")
			if len(infos) != 0 || len(errs) != 1 {
				t.Fatalf("List(%q/): %d items, %d errors (want exactly one error)", bad, len(infos), len(errs))
			}
		}
	})

	t.Run("PreCanceledContext", func(t *testing.T) {
		s := factory(t)
		must(t, s.Put(context.Background(), "k", strings.NewReader("x")))
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.Put(canceled, "k2", strings.NewReader("x")); !errors.Is(err, context.Canceled) {
			t.Fatalf("Put: %v", err)
		}
		if _, err := s.Open(canceled, "k"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Open: %v", err)
		}
		if _, err := s.Stat(canceled, "k"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Stat: %v", err)
		}
		if err := s.Delete(canceled, "k"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Delete: %v", err)
		}
		infos, errs := drainList(s, canceled, "")
		if len(infos) != 0 || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
			t.Fatalf("canceled List: %d items, errs %v (want exactly one canceled error)", len(infos), errs)
		}
		// Refused operations have no side effects.
		if _, err := s.Open(ctx, "k2"); !errors.Is(err, storage.ErrNotExist) {
			t.Fatalf("canceled Put stored an object: %v", err)
		}
		if got := read(t, s, "k"); got != "x" {
			t.Fatalf("canceled Delete removed the object: %q", got)
		}
	})

	t.Run("CanceledListOnEmptyStore", func(t *testing.T) {
		s := factory(t)
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		infos, errs := drainList(s, canceled, "")
		if len(infos) != 0 || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
			t.Fatalf("canceled List on empty store: %d items, errs %v", len(infos), errs)
		}
	})

	t.Run("CancelMidList", func(t *testing.T) {
		s := factory(t)
		for _, k := range []string{"m/1", "m/2", "m/3"} {
			must(t, s.Put(ctx, k, strings.NewReader(k)))
		}
		mid, cancel := context.WithCancel(context.Background())
		defer cancel()
		var infos []storage.Info
		var errs []error
		for info, err := range s.List(mid, "m/") {
			if err != nil {
				errs = append(errs, err)
				continue
			}
			infos = append(infos, info)
			cancel()
		}
		if len(infos) != 1 || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
			t.Fatalf("cancel after first item: %d items, errs %v (want one item, one canceled error, then stop)", len(infos), errs)
		}
	})

	t.Run("MixedOpsOnSharedPrefixDoNotStarve", func(t *testing.T) {
		s := factory(t)
		var wg sync.WaitGroup
		stop := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r := io.MultiReader(strings.NewReader("x"), errReader{})
				s.Put(ctx, "shared/failing", r)
				s.Delete(ctx, "shared/failing")
			}
		}()
		for i := 0; i < 30; i++ {
			k := fmt.Sprintf("shared/keep-%02d", i)
			if err := s.Put(ctx, k, strings.NewReader(k)); err != nil {
				close(stop)
				wg.Wait()
				t.Fatalf("valid put starved by concurrent failing ops: %v", err)
			}
			if got := read(t, s, k); got != k {
				close(stop)
				wg.Wait()
				t.Fatalf("key %s = %q", k, got)
			}
		}
		close(stop)
		wg.Wait()
	})

	t.Run("DeleteFreesPrefix", func(t *testing.T) {
		s := factory(t)
		must(t, s.Put(ctx, "a/b", strings.NewReader("x")))
		must(t, s.Delete(ctx, "a/b"))
		if err := s.Put(ctx, "a", strings.NewReader("y")); err != nil {
			t.Fatalf("key a blocked after a/b deleted: %v", err)
		}
		if got := read(t, s, "a"); got != "y" {
			t.Fatalf("a = %q", got)
		}
	})

	t.Run("MetacharacterKeysRoundTrip", func(t *testing.T) {
		s := factory(t)
		for _, key := range []string{
			"space in name.txt",
			"query?&#frag",
			"percent%41.txt",
			"unicode/üñíçødé.bin",
			"plus+and=eq",
		} {
			if err := s.Put(ctx, key, strings.NewReader(key)); err != nil {
				t.Fatalf("Put %q: %v", key, err)
			}
			if got := read(t, s, key); got != key {
				t.Fatalf("round trip %q = %q", key, got)
			}
			info, err := s.Stat(ctx, key)
			if err != nil || info.Key != key {
				t.Fatalf("Stat %q: %+v %v", key, info, err)
			}
			infos, errs := drainList(s, ctx, key)
			if len(errs) != 0 || len(infos) != 1 || infos[0].Key != key {
				t.Fatalf("List(%q): %v items, errs %v (metacharacter prefixes must list)", key, infos, errs)
			}
			must(t, s.Delete(ctx, key))
		}
	})

	t.Run("FailedPutLeavesNothing", func(t *testing.T) {
		s := factory(t)
		r := io.MultiReader(strings.NewReader("partial"), errReader{})
		if err := s.Put(ctx, "broken", r); err == nil {
			t.Fatal("Put with failing reader succeeded")
		}
		if _, err := s.Open(ctx, "broken"); !errors.Is(err, storage.ErrNotExist) {
			t.Fatalf("partial object visible after failed put: %v", err)
		}
		// A failed nested Put must not reserve its parent prefix.
		r = io.MultiReader(strings.NewReader("partial"), errReader{})
		if err := s.Put(ctx, "a/b", r); err == nil {
			t.Fatal("nested Put with failing reader succeeded")
		}
		must(t, s.Put(ctx, "a", strings.NewReader("y")))
		if got := read(t, s, "a"); got != "y" {
			t.Fatalf("key a blocked after failed nested put: %q", got)
		}
	})

	t.Run("InvalidListPrefixYieldsError", func(t *testing.T) {
		s := factory(t)
		infos, errs := drainList(s, ctx, "../up/")
		if len(infos) != 0 || len(errs) != 1 {
			t.Fatalf("invalid prefix: %d items, %d errors (want exactly one error, nothing else)", len(infos), len(errs))
		}
	})

	t.Run("PrefixKeysDontAliasDirectories", func(t *testing.T) {
		s := factory(t)
		must(t, s.Put(ctx, "a/b", strings.NewReader("blob")))
		if _, err := s.Open(ctx, "a"); !errors.Is(err, storage.ErrNotExist) {
			t.Fatalf("Open of a prefix directory: %v (a directory is not a blob)", err)
		}
		if _, err := s.Stat(ctx, "a"); !errors.Is(err, storage.ErrNotExist) {
			t.Fatalf("Stat of a prefix directory: %v", err)
		}
		must(t, s.Delete(ctx, "a"))
		if got := read(t, s, "a/b"); got != "blob" {
			t.Fatalf("Delete of a prefix key touched a/b: %q", got)
		}
		if _, err := s.Open(ctx, "a/b/c"); !errors.Is(err, storage.ErrNotExist) {
			t.Fatalf("Open below an existing blob: %v", err)
		}
		if _, err := s.Stat(ctx, "a/b/c"); !errors.Is(err, storage.ErrNotExist) {
			t.Fatalf("Stat below an existing blob: %v", err)
		}
	})

	t.Run("CancelStopsEndlessRead", func(t *testing.T) {
		s := factory(t)
		reads, cancel := context.WithCancel(context.Background())
		started := make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- s.Put(reads, "endless", &endlessReader{started: started}) }()
		<-started // wait until the first chunk has been read
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("endless put: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("canceled put never returned; backend does not check ctx between chunks")
		}
		if _, err := s.Open(ctx, "endless"); !errors.Is(err, storage.ErrNotExist) {
			t.Fatalf("canceled endless put stored an object: %v", err)
		}
	})

	t.Run("CancelDuringRead", func(t *testing.T) {
		s := factory(t)
		reads, cancel := context.WithCancel(context.Background())
		r := io.MultiReader(strings.NewReader("first"), cancelReader{cancel: cancel})
		err := s.Put(reads, "mid", r)
		if err == nil {
			t.Fatal("Put whose reader canceled the ctx succeeded")
		}
		if _, err := s.Open(ctx, "mid"); !errors.Is(err, storage.ErrNotExist) {
			t.Fatalf("object visible after canceled put: %v", err)
		}
	})

	t.Run("ConcurrentPutsDistinctKeys", func(t *testing.T) {
		s := factory(t)
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				k := "c/" + string(rune('a'+i))
				if err := s.Put(ctx, k, strings.NewReader(k)); err != nil {
					t.Error(err)
					return
				}
				rc, err := s.Open(ctx, k)
				if err != nil {
					t.Error(err)
					return
				}
				b, err := io.ReadAll(rc)
				rc.Close()
				if err != nil || string(b) != k {
					t.Errorf("key %s = %q (%v)", k, b, err)
				}
			}(i)
		}
		wg.Wait()
	})
}

// drainList consumes all yields, including trailing errors.
func drainList(s storage.Storage, ctx context.Context, prefix string) (infos []storage.Info, errs []error) {
	for info, err := range s.List(ctx, prefix) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		infos = append(infos, info)
	}
	return infos, errs
}

// endlessReader requires between-read context checks to stop Put.
type endlessReader struct {
	started chan struct{}
	once    sync.Once
}

func (e *endlessReader) Read(p []byte) (int, error) {
	e.once.Do(func() { close(e.started) })
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("reader failed") }

// cancelReader verifies that Put checks ctx after EOF and before commit.
type cancelReader struct{ cancel context.CancelFunc }

func (c cancelReader) Read([]byte) (int, error) {
	c.cancel()
	return 0, io.EOF
}

func read(t *testing.T, s storage.Storage, key string) string {
	t.Helper()
	rc, err := s.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
