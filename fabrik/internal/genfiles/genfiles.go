// Package genfiles writes generated file sets transactionally.
// Pruning deletes only manifest-owned files whose hashes still match.
package genfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ManifestName is the ownership record beside the generated files.
const ManifestName = "fabrik.manifest.json"

const journalName = "fabrik.manifest.journal.json"

type manifest struct {
	Version int               `json:"version"`
	Files   map[string]string `json:"files"`
	// Temps and Prune record staged writes and hash-guarded deletions.
	Temps map[string]string `json:"temps,omitempty"`
	Prune map[string]string `json:"prune,omitempty"`
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// validNames restricts generated files and temps to the output directory.
func validNames(files map[string]string) error {
	for name, v := range files {
		for _, n := range []string{name, v} {
			if n == "" || n != filepath.Base(n) || n == "." || n == ".." {
				return fmt.Errorf("entry %q is not a plain filename", n)
			}
		}
	}
	return nil
}

// validTempName accepts only staged names created by writeExclusive.
func validTempName(name string) bool {
	return name == filepath.Base(name) &&
		strings.HasPrefix(name, ".fabrik-") && strings.HasSuffix(name, ".tmp")
}

// validSums checks key names only; values are content hashes.
func validSums(files map[string]string) error {
	for name := range files {
		if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
			return fmt.Errorf("entry %q is not a plain filename", name)
		}
	}
	return nil
}

func readManifest(dir string) (manifest, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestName)) // #nosec G304 -- module-relative ownership record
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return manifest{}, false, nil
		}
		return manifest{}, false, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, false, fmt.Errorf("%s: %w", ManifestName, err)
	}
	if err := validSums(m.Files); err != nil {
		return manifest{}, false, fmt.Errorf("%s: %w", ManifestName, err)
	}
	return m, true, nil
}

func readJournal(dir string) (manifest, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, journalName)) // #nosec G304
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return manifest{}, false, nil
		}
		return manifest{}, false, err
	}
	var j manifest
	if err := json.Unmarshal(data, &j); err != nil {
		return manifest{}, false, fmt.Errorf("%s: %w", journalName, err)
	}
	if err := validSums(j.Files); err != nil {
		return manifest{}, false, fmt.Errorf("%s: %w", journalName, err)
	}
	if err := validNames(j.Temps); err != nil {
		return manifest{}, false, fmt.Errorf("%s: %w", journalName, err)
	}
	if err := validSums(j.Prune); err != nil {
		return manifest{}, false, fmt.Errorf("%s: %w", journalName, err)
	}
	return j, true, nil
}

func newManifest(files map[string][]byte) manifest {
	m := manifest{Version: 1, Files: map[string]string{}}
	for name, data := range files {
		m.Files[name] = hashOf(data)
	}
	return m
}

func sortedNames[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeExclusive writes data to a fresh temp created exclusively in
// dir, so a planted symlink cannot redirect the write; it syncs and
// returns the temp's base name.
func writeExclusive(dir string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".fabrik-*.tmp")
	if err != nil {
		return "", err
	}
	name := filepath.Base(f.Name())
	if err := f.Chmod(0o644); err != nil { // #nosec G302 -- generated source and its manifest are intentionally readable
		f.Close()
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return name, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EINVAL) {
		return err
	}
	return nil
}

// publish replaces dir/base through an exclusive temp and rename.
func publish(dir, base string, data []byte) error {
	tmp, err := writeExclusive(dir, data)
	if err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(dir, tmp), filepath.Join(dir, base)); err != nil {
		return err
	}
	return syncDir(dir)
}

// stage writes exclusive temps and persists the journal before any rename.
func stage(dir string, files map[string][]byte, withManifest bool) error {
	return stagePrune(dir, files, nil, withManifest)
}

func stagePrune(dir string, files map[string][]byte, prune map[string]string, withManifest bool) error {
	j := newManifest(files)
	j.Prune = prune
	j.Temps = map[string]string{}
	for _, name := range sortedNames(files) {
		tmp, err := writeExclusive(dir, files[name])
		if err != nil {
			return err
		}
		j.Temps[name] = tmp
	}
	if !withManifest {
		// The journal remains authoritative until all renames finish.
		j.Version = -j.Version
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return publish(dir, journalName, append(data, '\n'))
}

// commit applies staged writes and pruning, commits the manifest state, then removes the journal.
func commit(dir string, j manifest) (pruned, kept []string, err error) {
	withManifest := j.Version > 0
	for _, name := range sortedNames(j.Files) {
		if tmp, ok := j.Temps[name]; ok && validTempName(tmp) {
			tmpPath := filepath.Join(dir, tmp)
			if info, err := os.Lstat(tmpPath); err == nil && info.Mode().IsRegular() {
				// The staged content must be what the journal promised; a
				// crafted mapping cannot rename an arbitrary file away.
				data, rerr := os.ReadFile(tmpPath) // #nosec G304
				if rerr != nil {
					return nil, nil, rerr
				}
				if hashOf(data) != j.Files[name] {
					return nil, nil, fmt.Errorf("staged %s does not match the journal; regenerate with fabrik wire", name)
				}
				if err := os.Rename(tmpPath, filepath.Join(dir, name)); err != nil {
					return nil, nil, err
				}
				continue
			}
		}
		// A missing temp is valid only when an interrupted run installed the expected content.
		data, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304
		if err != nil || hashOf(data) != j.Files[name] {
			return nil, nil, fmt.Errorf("interrupted write left %s incomplete; regenerate with fabrik wire", name)
		}
	}
	for _, name := range sortedNames(j.Prune) {
		path := filepath.Join(dir, name)
		data, rerr := os.ReadFile(path) // #nosec G304
		if rerr != nil {
			if errors.Is(rerr, fs.ErrNotExist) {
				continue
			}
			return nil, nil, rerr
		}
		if hashOf(data) != j.Prune[name] {
			kept = append(kept, name)
			continue
		}
		if rerr := os.Remove(path); rerr != nil {
			return nil, nil, rerr
		}
		pruned = append(pruned, name)
	}
	if withManifest {
		m := manifest{Version: 1, Files: j.Files}
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return nil, nil, err
		}
		if err := publish(dir, ManifestName, append(data, '\n')); err != nil {
			return nil, nil, err
		}
	} else if err := os.Remove(filepath.Join(dir, ManifestName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	// Persist all committed state before removing the recovery record.
	if err := syncDir(dir); err != nil {
		return nil, nil, err
	}
	if err := os.Remove(filepath.Join(dir, journalName)); err != nil {
		return nil, nil, err
	}
	return pruned, kept, syncDir(dir)
}

// WriteSet replaces files transactionally and prunes obsolete, unchanged files owned by the
// previous manifest. It returns drifted files in kept and updates the manifest last.
func WriteSet(dir string, files map[string][]byte, withManifest bool) (written, pruned, kept []string, err error) {
	if err := validSums(newManifest(files).Files); err != nil {
		return nil, nil, nil, err
	}
	rkept, err := RollForward(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	prev, _, err := readManifest(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	prune := map[string]string{}
	for name, sum := range prev.Files {
		if _, still := files[name]; !still {
			prune[name] = sum
		}
	}
	if err := stagePrune(dir, files, prune, withManifest); err != nil {
		return nil, nil, nil, err
	}
	j, ok, err := readJournal(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	if !ok {
		return nil, nil, nil, errors.New("staged journal missing")
	}
	pruned, kept, err = commit(dir, j)
	if err != nil {
		return nil, nil, nil, err
	}
	return sortedNames(files), pruned, append(rkept, kept...), nil
}

// RollForward completes an interrupted transaction when a journal
// exists; kept lists prune candidates preserved because they drifted.
func RollForward(dir string) (kept []string, err error) {
	j, ok, err := readJournal(dir)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	_, kept, err = commit(dir, j)
	return kept, err
}

// Check reports differences between the intended file set, disk, and manifest.
func Check(dir string, files map[string][]byte, withManifest bool) (problems, kept []string, err error) {
	// Check reports pending transactions without modifying them.
	if _, ok, err := readJournal(dir); err != nil {
		return nil, nil, err
	} else if ok {
		problems = append(problems, "an interrupted write is pending; run fabrik wire")
	}
	for _, name := range sortedNames(files) {
		disk, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				problems = append(problems, name+" does not exist")
				continue
			}
			return nil, nil, err
		}
		if hashOf(disk) != hashOf(files[name]) {
			problems = append(problems, name+" is stale")
		}
	}
	prev, ok, err := readManifest(dir)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		for _, name := range sortedNames(prev.Files) {
			if _, still := files[name]; !still {
				problems = append(problems, name+" is no longer generated; run fabrik wire to prune it")
			}
		}
	}
	switch {
	case withManifest && !ok:
		problems = append(problems, ManifestName+" is missing")
	case withManifest && ok:
		want := newManifest(files)
		for _, name := range sortedNames(want.Files) {
			if prev.Files[name] != want.Files[name] {
				problems = append(problems, ManifestName+" is stale")
				break
			}
		}
	case !withManifest && ok:
		problems = append(problems, ManifestName+" is no longer needed; run fabrik wire to remove it")
	}
	return problems, kept, nil
}

// Owned lists files tracked by the manifest or a pending journal.
func Owned(dir string) []string {
	names := map[string]bool{}
	if m, ok, err := readManifest(dir); err == nil && ok {
		for name := range m.Files {
			names[name] = true
		}
	}
	if j, ok, err := readJournal(dir); err == nil && ok {
		for name := range j.Files {
			names[name] = true
		}
		for name := range j.Prune {
			names[name] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	return sortedNames(names)
}
