// Package storage stores blobs in a flat, slash-separated key namespace across
// pluggable backends.
package storage

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"strings"
	"time"
)

// ErrNotExist reports a missing key and aliases fs.ErrNotExist.
var ErrNotExist = fs.ErrNotExist

// Info describes a stored blob.
type Info struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// Storage streams blobs by key with atomic replacement that preserves open
// readers, idempotent deletion, lexically ordered prefix listing, caller-owned
// Put readers, and context checks between reads and before commit; callers must
// use context-aware readers because cancellation cannot interrupt a blocked Read.
type Storage interface {
	Put(ctx context.Context, key string, r io.Reader) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (Info, error)
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) iter.Seq2[Info, error]
}

// CheckKey validates a non-empty slash-separated key with non-empty,
// non-dot-prefixed segments and no backslashes or control bytes.
func CheckKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return fmt.Errorf("invalid key %q", key)
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return fmt.Errorf("invalid key %q", key)
		}
	}
	// Backslashes and control bytes are not portable across backends.
	for i := 0; i < len(key); i++ {
		if key[i] == '\\' || key[i] < 0x20 || key[i] == 0x7f {
			return fmt.Errorf("invalid key %q", key)
		}
	}
	return nil
}

func checkPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return CheckKey(strings.TrimSuffix(prefix, "/"))
}

func opCheck(op, key string, ctx context.Context) error {
	if err := CheckKey(key); err != nil {
		return fmt.Errorf("storage: %s %q: %w", op, key, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("storage: %s %q: %w", op, key, err)
	}
	return nil
}

// copyChunks checks ctx between reads.
func copyChunks(ctx context.Context, dst io.Writer, src io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
