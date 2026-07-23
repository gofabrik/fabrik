package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is a process-local Storage for tests and development. Put replaces
// values atomically, and open readers retain their version.
type Memory struct {
	mu  sync.RWMutex
	m   map[string]memBlob
	now func() time.Time
}

type memBlob struct {
	data    []byte
	modTime time.Time
}

// NewMemory returns an empty Memory.
func NewMemory() *Memory { return &Memory{m: map[string]memBlob{}, now: time.Now} }

func (s *Memory) Put(ctx context.Context, key string, r io.Reader) error {
	if err := opCheck("put", key, ctx); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := copyChunks(ctx, &buf, r); err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("storage: put %q: %w", key, err)
	}
	data := buf.Bytes()
	s.mu.Lock()
	s.m[key] = memBlob{data: data, modTime: s.now()}
	s.mu.Unlock()
	return nil
}

func (s *Memory) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := opCheck("open", key, ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	b, ok := s.m[key]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("storage: open %q: %w", key, ErrNotExist)
	}
	// Preserve Seek support for range requests.
	return memReader{bytes.NewReader(b.data)}, nil
}

type memReader struct{ *bytes.Reader }

func (memReader) Close() error { return nil }

func (s *Memory) Stat(ctx context.Context, key string) (Info, error) {
	if err := opCheck("stat", key, ctx); err != nil {
		return Info{}, err
	}
	s.mu.RLock()
	b, ok := s.m[key]
	s.mu.RUnlock()
	if !ok {
		return Info{}, fmt.Errorf("storage: stat %q: %w", key, ErrNotExist)
	}
	return Info{Key: key, Size: int64(len(b.data)), ModTime: b.modTime}, nil
}

func (s *Memory) Delete(ctx context.Context, key string) error {
	if err := opCheck("delete", key, ctx); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
	return nil
}

func (s *Memory) List(ctx context.Context, prefix string) iter.Seq2[Info, error] {
	return func(yield func(Info, error) bool) {
		if err := checkPrefix(prefix); err != nil {
			yield(Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
			return
		}
		if err := ctx.Err(); err != nil {
			yield(Info{}, fmt.Errorf("storage: list %q: %w", prefix, err))
			return
		}
		s.mu.RLock()
		infos := make([]Info, 0, len(s.m))
		for k, b := range s.m {
			if strings.HasPrefix(k, prefix) {
				infos = append(infos, Info{Key: k, Size: int64(len(b.data)), ModTime: b.modTime})
			}
		}
		s.mu.RUnlock()
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
