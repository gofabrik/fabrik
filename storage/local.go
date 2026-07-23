package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
)

// Local stores blobs beneath an os.Root so keys and symlinks cannot escape,
// using atomic rename so open readers retain their version.
type Local struct {
	root *os.Root
	// mu prevents pruning a parent between Put's MkdirAll and Rename.
	mu sync.RWMutex
}

// NewLocal opens dir as the storage root. The directory must exist.
func NewLocal(dir string) (*Local, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("storage: open root: %w", err)
	}
	return &Local{root: root}, nil
}

// Close releases the root handle.
func (s *Local) Close() error { return s.root.Close() }

func (s *Local) Put(ctx context.Context, key string, r io.Reader) error {
	if err := opCheck("put", key, ctx); err != nil {
		return err
	}
	if dir := path.Dir(key); dir != "." {
		if err := s.root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("storage: put %q: %w", key, err)
		}
	}
	// The reserved dot namespace prevents .tmp from colliding with keys.
	if err := s.root.MkdirAll(".tmp", 0o755); err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	fail := func(err error) error {
		s.pruneParents(key)
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	suffix, err := randSuffix()
	if err != nil {
		return fail(err)
	}
	tmp := ".tmp/" + suffix
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fail(err)
	}
	if err := copyChunks(ctx, f, r); err != nil {
		f.Close()
		s.root.Remove(tmp)
		return fail(err)
	}
	if err := f.Close(); err != nil {
		s.root.Remove(tmp)
		return fail(err)
	}
	// The final read can cancel ctx and return EOF.
	if err := ctx.Err(); err != nil {
		s.root.Remove(tmp)
		return fail(err)
	}
	// Hold the read lock across MkdirAll and Rename so pruning cannot remove the parent.
	s.mu.RLock()
	var renameErr error
	if dir := path.Dir(key); dir != "." {
		renameErr = s.root.MkdirAll(dir, 0o755)
	}
	if renameErr == nil {
		renameErr = s.root.Rename(tmp, key)
	}
	s.mu.RUnlock()
	if renameErr == nil {
		return nil
	}
	s.root.Remove(tmp)
	return fail(renameErr)
}

// pruneParents removes empty parent directories without deleting blobs or
// racing a Put commit.
func (s *Local) pruneParents(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for dir := path.Dir(key); dir != "."; dir = path.Dir(dir) {
		fi, err := s.root.Lstat(dir)
		if err != nil || !fi.IsDir() {
			return
		}
		if err := s.root.Remove(dir); err != nil {
			return
		}
	}
}

func (s *Local) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := opCheck("open", key, ctx); err != nil {
		return nil, err
	}
	fi, err := s.root.Stat(key)
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", key, pathErr(err))
	}
	if fi.IsDir() {
		// A directory is other keys' namespace, never a blob.
		return nil, fmt.Errorf("storage: open %q: %w", key, ErrNotExist)
	}
	f, err := s.root.Open(key)
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", key, pathErr(err))
	}
	return f, nil
}

func (s *Local) Stat(ctx context.Context, key string) (Info, error) {
	if err := opCheck("stat", key, ctx); err != nil {
		return Info{}, err
	}
	fi, err := s.root.Stat(key)
	if err != nil {
		return Info{}, fmt.Errorf("storage: stat %q: %w", key, pathErr(err))
	}
	if fi.IsDir() {
		return Info{}, fmt.Errorf("storage: stat %q: %w", key, ErrNotExist)
	}
	return Info{Key: key, Size: fi.Size(), ModTime: fi.ModTime()}, nil
}

func (s *Local) Delete(ctx context.Context, key string) error {
	if err := opCheck("delete", key, ctx); err != nil {
		return err
	}
	fi, err := s.root.Lstat(key)
	if err != nil {
		if isAbsent(err) {
			return nil
		}
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	if fi.IsDir() {
		// The key holds no blob; the directory belongs to other keys.
		return nil
	}
	if err := s.root.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	s.pruneParents(key)
	return nil
}

func (s *Local) List(ctx context.Context, prefix string) iter.Seq2[Info, error] {
	return func(yield func(Info, error) bool) {
		if err := checkPrefix(prefix); err != nil {
			yield(Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
			return
		}
		if err := ctx.Err(); err != nil {
			yield(Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
			return
		}
		var infos []Info
		err := fs.WalkDir(s.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p == ".tmp" {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(p, prefix) {
				fi, err := d.Info()
				if err != nil {
					return err
				}
				infos = append(infos, Info{Key: p, Size: fi.Size(), ModTime: fi.ModTime()})
			}
			return ctx.Err()
		})
		if err != nil {
			yield(Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
			return
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
		for _, info := range infos {
			if err := ctx.Err(); err != nil {
				yield(Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
				return
			}
			if !yield(info, nil) {
				return
			}
		}
	}
}

// pathErr maps path-prefix conflicts to ErrNotExist and unwraps fs.PathError.
func pathErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	if errors.Is(err, syscall.ENOTDIR) {
		return ErrNotExist
	}
	return err
}

func isAbsent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func randSuffix() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
