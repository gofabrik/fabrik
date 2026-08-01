package genfiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestWriteSetWritesFilesAndManifest(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"main.gen.go":              []byte("package main\n"),
		"fragments_builddb.gen.go": []byte("package main\n\nfunc buildDb() {}\n"),
	}
	written, pruned, kept, err := WriteSet(dir, files, true)
	if err != nil {
		t.Fatalf("WriteSet: %v", err)
	}
	if len(written) != 2 || len(pruned) != 0 || len(kept) != 0 {
		t.Fatalf("written=%v pruned=%v kept=%v", written, pruned, kept)
	}
	if read(t, filepath.Join(dir, "main.gen.go")) != "package main\n" {
		t.Fatal("main content wrong")
	}
	if !strings.Contains(read(t, filepath.Join(dir, ManifestName)), "fragments_builddb.gen.go") {
		t.Fatal("manifest missing entry")
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); !os.IsNotExist(err) {
		t.Fatal("journal left behind")
	}
}

func TestWriteSetPrunesOwnedFilesOnly(t *testing.T) {
	dir := t.TempDir()
	first := map[string][]byte{
		"main.gen.go":              []byte("package main\n"),
		"fragments_builddb.gen.go": []byte("package main\n\nfunc buildDb() {}\n"),
	}
	if _, _, _, err := WriteSet(dir, first, true); err != nil {
		t.Fatalf("first WriteSet: %v", err)
	}
	// Pruning preserves a manually changed fragment.
	drifted := filepath.Join(dir, "fragments_builddb.gen.go")
	if err := os.WriteFile(drifted, []byte("package main // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := map[string][]byte{"main.gen.go": []byte("package main\n")}
	_, pruned, kept, err := WriteSet(dir, second, false)
	if err != nil {
		t.Fatalf("second WriteSet: %v", err)
	}
	if len(pruned) != 0 || len(kept) != 1 {
		t.Fatalf("pruned=%v kept=%v, want the drifted file kept", pruned, kept)
	}
	if _, err := os.Stat(drifted); err != nil {
		t.Fatal("drifted file deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, ManifestName)); !os.IsNotExist(err) {
		t.Fatal("manifest must be removed in single-file mode")
	}
}

func TestWriteSetPrunesMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	first := map[string][]byte{
		"main.gen.go":              []byte("package main\n"),
		"fragments_builddb.gen.go": []byte("package main\n\nfunc buildDb() {}\n"),
	}
	if _, _, _, err := WriteSet(dir, first, true); err != nil {
		t.Fatalf("first WriteSet: %v", err)
	}
	second := map[string][]byte{"main.gen.go": []byte("package main\n")}
	_, pruned, kept, err := WriteSet(dir, second, true)
	if err != nil {
		t.Fatalf("second WriteSet: %v", err)
	}
	if len(pruned) != 1 || len(kept) != 0 {
		t.Fatalf("pruned=%v kept=%v", pruned, kept)
	}
	if _, err := os.Stat(filepath.Join(dir, "fragments_builddb.gen.go")); !os.IsNotExist(err) {
		t.Fatal("owned unchanged file not pruned")
	}
}

func TestRollForwardCompletesInterruptedWrite(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"main.gen.go":              []byte("package main\n"),
		"fragments_builddb.gen.go": []byte("package main\n\nfunc buildDb() {}\n"),
	}
	// Recovery completes a transaction staged before any rename.
	if err := stage(dir, files, true); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := RollForward(dir); err != nil {
		t.Fatalf("RollForward: %v", err)
	}
	if read(t, filepath.Join(dir, "main.gen.go")) != "package main\n" {
		t.Fatal("roll-forward missed main")
	}
	if !strings.Contains(read(t, filepath.Join(dir, ManifestName)), "main.gen.go") {
		t.Fatal("roll-forward missed the manifest")
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); !os.IsNotExist(err) {
		t.Fatal("journal left behind")
	}
}

func TestCheckReportsMissingStaleAndOrphans(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"main.gen.go":              []byte("package main\n"),
		"fragments_builddb.gen.go": []byte("package main\n\nfunc buildDb() {}\n"),
	}
	if _, _, _, err := WriteSet(dir, files, true); err != nil {
		t.Fatalf("WriteSet: %v", err)
	}
	if problems, _, err := Check(dir, files, true); err != nil || len(problems) != 0 {
		t.Fatalf("clean check = %v, %v", problems, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.gen.go"), []byte("package main // stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "fragments_builddb.gen.go")); err != nil {
		t.Fatal(err)
	}
	// An intended file absent from disk is missing.
	problems, _, err := Check(dir, files, true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "main.gen.go is stale") || !strings.Contains(joined, "fragments_builddb.gen.go does not exist") {
		t.Fatalf("problems = %v, want stale main and missing fragment", problems)
	}
	// A manifest-owned file omitted from the intended set is stale.
	smaller := map[string][]byte{"main.gen.go": files["main.gen.go"]}
	problems, _, err = Check(dir, smaller, true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	joined = strings.Join(problems, "\n")
	if !strings.Contains(joined, "no longer generated") {
		t.Fatalf("problems = %v, want the orphaned fragment reported", problems)
	}
}

func TestCheckManifestState(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{"main.gen.go": []byte("package main\n")}
	if _, _, _, err := WriteSet(dir, files, false); err != nil {
		t.Fatalf("WriteSet: %v", err)
	}
	problems, _, err := Check(dir, files, true)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], ManifestName+" is missing") {
		t.Fatalf("split without manifest = %v, %v", problems, err)
	}
	if _, _, _, err := WriteSet(dir, files, true); err != nil {
		t.Fatalf("WriteSet manifest: %v", err)
	}
	problems, _, err = Check(dir, files, false)
	if err != nil || len(problems) != 1 || !strings.Contains(problems[0], "no longer needed") {
		t.Fatalf("single with manifest = %v, %v", problems, err)
	}
	changed := map[string][]byte{"main.gen.go": []byte("package main // v2\n")}
	problems, _, err = Check(dir, changed, true)
	if err != nil || !strings.Contains(strings.Join(problems, "\n"), ManifestName+" is stale") {
		t.Fatalf("changed content = %v, %v, want stale manifest", problems, err)
	}
}

func TestRollForwardReportsDriftedPruneCandidate(t *testing.T) {
	dir := t.TempDir()
	both := map[string][]byte{
		"main.gen.go":              []byte("package main\n"),
		"fragments_builddb.gen.go": []byte("package main\n\nfunc buildDb() {}\n"),
	}
	if _, _, _, err := WriteSet(dir, both, true); err != nil {
		t.Fatalf("WriteSet: %v", err)
	}
	// Recovery preserves a prune candidate changed after staging.
	prune := map[string]string{"fragments_builddb.gen.go": hashOf(both["fragments_builddb.gen.go"])}
	if err := stagePrune(dir, map[string][]byte{"main.gen.go": both["main.gen.go"]}, prune, true); err != nil {
		t.Fatalf("stagePrune: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fragments_builddb.gen.go"), []byte("package main // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kept, err := RollForward(dir)
	if err != nil {
		t.Fatalf("RollForward: %v", err)
	}
	if len(kept) != 1 || kept[0] != "fragments_builddb.gen.go" {
		t.Fatalf("kept = %v, want the drifted candidate reported", kept)
	}
	if _, err := os.Stat(filepath.Join(dir, "fragments_builddb.gen.go")); err != nil {
		t.Fatal("drifted candidate deleted")
	}
}

func TestOwnedIncludesPendingJournal(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{"fragments_x.gen.go": []byte("package main\n")}
	if err := stagePrune(dir, files, map[string]string{"fragments_old.gen.go": "deadbeef"}, true); err != nil {
		t.Fatalf("stagePrune: %v", err)
	}
	owned := Owned(dir)
	joined := strings.Join(owned, ",")
	if !strings.Contains(joined, "fragments_x.gen.go") || !strings.Contains(joined, "fragments_old.gen.go") {
		t.Fatalf("Owned = %v, want journal files and prune candidates", owned)
	}
}

func TestOwnedListsManifestFiles(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{"main.gen.go": []byte("package main\n"), "fragments_x.gen.go": []byte("package main\n")}
	if _, _, _, err := WriteSet(dir, files, true); err != nil {
		t.Fatalf("WriteSet: %v", err)
	}
	owned := Owned(dir)
	if len(owned) != 2 {
		t.Fatalf("Owned = %v", owned)
	}
}

func TestCheckReportsPendingJournalWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{"main.gen.go": []byte("package main\n")}
	if err := stage(dir, files, true); err != nil {
		t.Fatalf("stage: %v", err)
	}
	problems, _, err := Check(dir, files, true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(problems) == 0 || !strings.Contains(problems[0], "interrupted write is pending") {
		t.Fatalf("problems = %v, want the pending journal reported", problems)
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); err != nil {
		t.Fatal("check must leave the journal for the next wire run")
	}
	if _, err := os.Stat(filepath.Join(dir, "main.gen.go")); !os.IsNotExist(err) {
		t.Fatal("check must not publish staged files")
	}
}

func TestCheckReportsDriftedOrphansAsKept(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{
		"main.gen.go":              []byte("package main\n"),
		"fragments_builddb.gen.go": []byte("package main\n\nfunc buildDb() {}\n"),
	}
	if _, _, _, err := WriteSet(dir, files, true); err != nil {
		t.Fatalf("WriteSet: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fragments_builddb.gen.go"), []byte("package main // edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	smaller := map[string][]byte{"main.gen.go": files["main.gen.go"]}
	problems, kept, err := Check(dir, smaller, true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(kept) != 1 || kept[0] != "fragments_builddb.gen.go" {
		t.Fatalf("kept = %v, want the edited orphan", kept)
	}
	joined := strings.Join(problems, "\n")
	if strings.Contains(joined, "fragments_builddb.gen.go is no longer generated;") {
		t.Fatalf("problems = %v, want the edited orphan reported only through kept", problems)
	}
}

func TestRollForwardAbandonsUnrecoverableJournal(t *testing.T) {
	dir := t.TempDir()
	files := map[string][]byte{"main.gen.go": []byte("package main\n")}
	if err := stage(dir, files, true); err != nil {
		t.Fatalf("stage: %v", err)
	}
	// Corrupt the staged temp: the journal can never complete.
	j, ok, err := readJournal(dir)
	if err != nil || !ok {
		t.Fatalf("readJournal: %v, %v", ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, j.Temps["main.gen.go"]), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RollForward(dir); err != nil {
		t.Fatalf("RollForward: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, journalName)); !os.IsNotExist(err) {
		t.Fatal("abandon must remove the journal")
	}
	if _, err := os.Stat(filepath.Join(dir, j.Temps["main.gen.go"])); !os.IsNotExist(err) {
		t.Fatal("abandon must remove staged temps")
	}
	// The next write starts clean and succeeds.
	if _, _, _, err := WriteSet(dir, files, true); err != nil {
		t.Fatalf("WriteSet after abandon: %v", err)
	}
	if read(t, filepath.Join(dir, "main.gen.go")) != "package main\n" {
		t.Fatal("recovery write missed the file")
	}
}
