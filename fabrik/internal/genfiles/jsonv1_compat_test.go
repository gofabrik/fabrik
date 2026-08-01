package genfiles

import (
	jsonv1 "encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Manifest and journal output preserves v1 ordering, indentation, and trailing newline.
func TestManifestAndJournalKeepV1Bytes(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"zz.gen.go":     []byte("package main\n"),
		"aa.gen.go":     []byte("package main\n"),
		"m<&>it.gen.go": []byte("package main\n"),
	}
	if err := stage(dir, files, true); err != nil {
		t.Fatalf("stage: %v", err)
	}
	j, ok, err := readJournal(dir)
	if err != nil || !ok {
		t.Fatalf("readJournal: %v %v", ok, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, journalName))
	if err != nil {
		t.Fatal(err)
	}
	want, err := jsonv1.MarshalIndent(j, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want)+"\n" {
		t.Fatalf("journal bytes differ from v1 shape:\n got: %q\nwant: %q", raw, string(want)+"\n")
	}
	if _, _, err := commit(dir, j); err != nil {
		t.Fatalf("commit: %v", err)
	}
	rawM, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	m, ok, err := readManifest(dir)
	if err != nil || !ok {
		t.Fatalf("readManifest: %v %v", ok, err)
	}
	wantM, err := jsonv1.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(rawM) != string(wantM)+"\n" {
		t.Fatalf("manifest bytes differ from v1 shape:\n got: %q\nwant: %q", rawM, string(wantM)+"\n")
	}
}
