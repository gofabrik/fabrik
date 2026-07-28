package assetmapper

import (
	"fmt"
	"os"
	"path/filepath"
)

// IndeterminateCommitError means an atomic rename succeeded but syncing its
// parent directory failed. The new file is visible, but crash durability could
// not be confirmed.
type IndeterminateCommitError struct {
	Path string
	Err  error
}

func (e *IndeterminateCommitError) Error() string {
	return fmt.Sprintf("assetmapper: %s was replaced but directory sync failed: %v", e.Path, e.Err)
}

func (e *IndeterminateCommitError) Unwrap() error { return e.Err }

// atomicWriteFile writes and syncs a sibling temporary file before renaming it
// over path. Errors before rename leave the previous file untouched; errors
// after rename are reported as [IndeterminateCommitError].
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tmp)
	}
	if err := file.Chmod(perm); err != nil {
		cleanup()
		return err
	}
	if _, err := file.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return &IndeterminateCommitError{Path: path, Err: err}
	}
	return nil
}

func syncAncestorDirectories(leaf, stop string) error {
	leaf = filepath.Clean(leaf)
	stop = filepath.Clean(stop)
	rel, err := filepath.Rel(stop, leaf)
	if err != nil || rel == ".." || filepath.IsAbs(rel) ||
		len(rel) > 3 && rel[:3] == ".."+string(filepath.Separator) {
		return fmt.Errorf("%s is outside %s", leaf, stop)
	}
	for current := leaf; ; current = filepath.Dir(current) {
		if err := syncDirectory(current); err != nil {
			return err
		}
		if current == stop {
			return nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("reached filesystem root before %s", stop)
		}
	}
}

func syncDirectory(path string) error {
	dir, err := os.Open(path) // #nosec G304 -- caller-selected output directory
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // the explicit sync determines durability
	return dir.Sync()
}
