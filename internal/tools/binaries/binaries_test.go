package binaries

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteArchiveIsReproducible(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fabrik")
	if err := os.WriteFile(bin, []byte("fake-binary-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(dir, "a.tar.gz"), filepath.Join(dir, "b.tar.gz")
	sa, err := writeArchive(a, bin)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := writeArchive(b, bin)
	if err != nil {
		t.Fatal(err)
	}
	if sa != sb {
		t.Fatalf("checksums differ across builds: %s vs %s", sa, sb)
	}
	// #nosec G304 -- reads a test-controlled temporary path
	da, _ := os.ReadFile(a)
	// #nosec G304 -- reads a test-controlled temporary path
	db, _ := os.ReadFile(b)
	if !bytes.Equal(da, db) {
		t.Fatal("archives are not byte-identical for identical input")
	}
}

func TestWriteArchiveHoldsExecutable(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fabrik")
	if err := os.WriteFile(bin, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	arch := filepath.Join(dir, "out.tar.gz")
	if _, err := writeArchive(arch, bin); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- reads a test-controlled temporary path
	f, err := os.Open(arch)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // closing a read-only test fixture
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	hdr, err := tar.NewReader(gz).Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "fabrik" {
		t.Fatalf("archive entry = %q, want fabrik", hdr.Name)
	}
	if hdr.Mode&0o111 == 0 {
		t.Fatalf("binary not executable: mode %o", hdr.Mode)
	}
}
