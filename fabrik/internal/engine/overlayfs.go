package engine

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// overlayDirFS applies engine overlays to non-Go files during validation.
type overlayDirFS struct {
	dir     string
	overlay map[string][]byte
}

func (o overlayDirFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if data, ok := o.overlay[o.abs(name)]; ok {
		return &overlayFile{name: path.Base(name), Reader: bytes.NewReader(data)}, nil
	}
	return os.DirFS(o.dir).Open(name)
}

// ReadDir includes overlay-only files and directories.
func (o overlayDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	disk, diskErr := os.ReadDir(filepath.Join(o.dir, filepath.FromSlash(name)))

	entries := map[string]fs.DirEntry{}
	for _, e := range disk {
		entries[e.Name()] = e
	}
	prefix := o.abs(name) + string(filepath.Separator)
	overlaid := false
	for abs, data := range o.overlay {
		if !strings.HasPrefix(abs, prefix) {
			continue
		}
		overlaid = true
		rest := abs[len(prefix):]
		if i := strings.IndexByte(rest, filepath.Separator); i >= 0 {
			child := rest[:i]
			if _, onDisk := entries[child]; !onDisk {
				entries[child] = overlayDirEntry{name: child, dir: true}
			}
			continue
		}
		if _, onDisk := entries[rest]; !onDisk {
			entries[rest] = overlayDirEntry{name: rest, size: int64(len(data))}
		}
	}

	if diskErr != nil && !overlaid {
		return nil, diskErr
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]fs.DirEntry, len(names))
	for i, n := range names {
		out[i] = entries[n]
	}
	return out, nil
}

func (o overlayDirFS) abs(name string) string {
	return filepath.Join(o.dir, filepath.FromSlash(name))
}

type overlayFile struct {
	name string
	*bytes.Reader
}

func (f *overlayFile) Stat() (fs.FileInfo, error) {
	return overlayFileInfo{name: f.name, size: f.Reader.Size()}, nil
}
func (f *overlayFile) Close() error { return nil }

var _ io.ReaderAt = (*overlayFile)(nil)

type overlayDirEntry struct {
	name string
	size int64
	dir  bool
}

func (e overlayDirEntry) Name() string { return e.name }
func (e overlayDirEntry) IsDir() bool  { return e.dir }
func (e overlayDirEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e overlayDirEntry) Info() (fs.FileInfo, error) {
	return overlayFileInfo{name: e.name, size: e.size, dir: e.dir}, nil
}

type overlayFileInfo struct {
	name string
	size int64
	dir  bool
}

func (i overlayFileInfo) Name() string { return i.name }
func (i overlayFileInfo) Size() int64  { return i.size }
func (i overlayFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (i overlayFileInfo) ModTime() time.Time { return time.Time{} }
func (i overlayFileInfo) IsDir() bool        { return i.dir }
func (i overlayFileInfo) Sys() any           { return nil }
