package assetmapper

import (
	jsonv1 "encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Artifact output preserves v1 ordering, indentation, escaping, and trailing newline.
func TestManifestSaveKeepsV1Bytes(t *testing.T) {
	m := &Manifest{
		Entries:      map[string]string{"z.js": "z-1.js", "a.js": "a-2.js", "m<&>.js": "m-3.js"},
		Dependencies: map[string][]string{"z.js": {"a.js"}},
	}
	dir := t.TempDir()
	if err := m.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	want, err := jsonv1.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want)+"\n" {
		t.Fatalf("manifest bytes differ from v1 shape:\n got: %q\nwant: %q", got, string(want)+"\n")
	}
}

func TestImportmapWriteKeepsV1Bytes(t *testing.T) {
	im := &Importmap{Entries: map[string]ImportmapEntry{
		"zeta":  {Path: "vendor/zeta.js", Entrypoint: true},
		"alpha": {Version: "1.2.3"},
		"esc":   {Path: "vendor/<&>.js"},
	}}
	var b strings.Builder
	if err := im.Write(&b); err != nil {
		t.Fatalf("Write: %v", err)
	}
	want, err := jsonv1.MarshalIndent(im.Entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want)+"\n" {
		t.Fatalf("importmap bytes differ from v1 shape:\n got: %q\nwant: %q", b.String(), string(want)+"\n")
	}
}

// The rendered body and CSP hash must use identical v1 escaping.
func TestImportmapBodyEscapesHostileStrings(t *testing.T) {
	hostile := map[string]string{
		"</script><script>alert(1)</script>": "/v/x.js",
		"line\u2028sep":                      "/v/y\u2029.js",
		"bad\xffutf8":                        "/v/z.js",
	}
	keys := make([]string, 0, len(hostile))
	for k := range hostile {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	body := importmapBody(keys, hostile)
	for _, raw := range []string{"</script>", "\u2028", "\u2029"} {
		if strings.Contains(body, raw) {
			t.Fatalf("body contains raw %q:\n%s", raw, body)
		}
	}
	var check map[string]map[string]string
	if err := jsonv1.Unmarshal([]byte(body), &check); err != nil {
		t.Fatalf("body is not valid JSON: %v\n%s", err, body)
	}
	for k, v := range hostile {
		kb, _ := jsonv1.Marshal(k)
		vb, _ := jsonv1.Marshal(v)
		if !strings.Contains(body, string(kb)) || !strings.Contains(body, string(vb)) {
			t.Fatalf("body lost v1 escaping for %q/%q:\n%s", k, v, body)
		}
	}
}

func TestVendorLockSaveKeepsV1Bytes(t *testing.T) {
	l := &VendorLock{
		Version:            vendorLockVersion,
		DirectRequirements: map[string]string{"zeta": "2.0.0", "alpha": "1.0.0"},
		Packages: map[string]LockedPackage{
			"zeta":  {Version: "2.0.0", Type: "js", SourceURL: "https://x/<&>.tgz", SourceSize: 1, SourceSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", Size: 1, Owners: []string{"zeta"}, SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
			"alpha": {Version: "1.0.0", Type: "css", SourceURL: "https://x/a.tgz", SourceSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", Owners: []string{"alpha"}, SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		},
	}
	path := filepath.Join(t.TempDir(), "vendor.lock.json")
	if err := l.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := jsonv1.MarshalIndent(l, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want)+"\n" {
		t.Fatalf("lock bytes differ from v1 shape:\n got: %q\nwant: %q", raw, string(want)+"\n")
	}
}

// Permissive readers ignore trailing input, while the lock rejects it.
func TestDecodersKeepV1TrailingValueContract(t *testing.T) {
	im, err := ParseImportmap(strings.NewReader(`{"a":{"path":"x.js"}}{"trailing":true}`))
	if err != nil {
		t.Fatalf("ParseImportmap with trailing value: %v", err)
	}
	if _, ok := im.Entries["a"]; !ok {
		t.Fatal("first value not decoded")
	}
	if _, err := ParseImportmap(strings.NewReader(`{"a":{"unknown_field":1}}`)); err == nil {
		t.Fatal("unknown member accepted")
	}
	m, err := ParseManifest(strings.NewReader(`{"entries":{"a.js":"a-1.js"}}[1,2]`))
	if err != nil {
		t.Fatalf("ParseManifest with trailing value: %v", err)
	}
	if m.Entries["a.js"] != "a-1.js" {
		t.Fatal("manifest first value not decoded")
	}
	if _, err := ParseManifest(strings.NewReader(`{"unknown_key":1}`)); err != nil {
		t.Fatalf("manifest must stay permissive on unknown members: %v", err)
	}
}

func TestVendorLockDecodeContract(t *testing.T) {
	valid := func() []byte {
		l := validCompatLock()
		data, err := jsonv1.MarshalIndent(l, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(data, '\n')
	}
	write := func(data []byte) string {
		path := filepath.Join(t.TempDir(), "vendor.lock.json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if _, err := LoadVendorLock(write(valid())); err != nil {
		t.Fatalf("valid lock with trailing newline: %v", err)
	}
	if _, err := LoadVendorLock(write(append(valid(), " \n\t"...))); err != nil {
		t.Fatalf("whitespace after the value must stay accepted: %v", err)
	}
	if _, err := LoadVendorLock(write(append(valid(), '{', '}'))); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("second value = %v, want the multiple-values error", err)
	}
	if _, err := LoadVendorLock(write(append(valid(), '{'))); err == nil {
		t.Fatal("malformed trailing input accepted")
	}
	bad := strings.Replace(string(valid()), `"version": 2`, `"version": 2, "unknown_member": 1`, 1)
	if _, err := LoadVendorLock(write([]byte(bad))); err == nil {
		t.Fatal("unknown member accepted")
	}
}

func validCompatLock() *VendorLock {
	const h = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	return &VendorLock{
		Version:            vendorLockVersion,
		DirectRequirements: map[string]string{"alpha": "1.0.0"},
		Packages: map[string]LockedPackage{
			"alpha": {Version: "1.0.0", Type: "js", SourceURL: "https://x/a.tgz", SourceSHA256: h, Owners: []string{"alpha"}, SHA256: h},
		},
	}
}

// The journal retains v1 encoding when publication aborts.
func TestVendorTransactionJournalKeepsV1Bytes(t *testing.T) {
	dir := t.TempDir()
	v := &Vendor{VendorDir: filepath.Join(dir, "vendor"), Importmap: NewImportmap()}
	invalid := &VendorLock{Version: vendorLockVersion, DirectRequirements: map[string]string{}, Packages: map[string]LockedPackage{
		"bad": {Version: "1.0.0", Type: "js"},
	}}
	next := &Importmap{Entries: map[string]ImportmapEntry{"zeta": {Version: "1.0.0"}, "alpha": {Path: "a.js"}}}
	if err := v.publishMetadata(invalid, next); err == nil {
		t.Fatal("invalid lock published")
	}
	raw, err := os.ReadFile(v.transactionPath())
	if err != nil {
		t.Fatalf("journal missing after aborted publication: %v", err)
	}
	want, err := jsonv1.MarshalIndent(vendorTransaction{Version: 1, Lock: invalid, Entries: next.Entries}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(want)+"\n" {
		t.Fatalf("transaction bytes differ from v1 shape:\n got: %q\nwant: %q", raw, string(want)+"\n")
	}
}

// Recovery accepts trailing whitespace but rejects unknown members and trailing values.
func TestVendorTransactionDecodeContract(t *testing.T) {
	journal := func(t *testing.T) ([]byte, *Vendor) {
		t.Helper()
		dir := t.TempDir()
		v := &Vendor{VendorDir: filepath.Join(dir, "vendor"), Importmap: NewImportmap()}
		if err := os.MkdirAll(v.VendorDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// The empty-content hash lets recovery verify an empty vendored file.
		if err := os.WriteFile(filepath.Join(v.VendorDir, "alpha.js"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		tx := vendorTransaction{Version: 1, Lock: validCompatLock(), Entries: map[string]ImportmapEntry{"alpha": {Version: "1.0.0"}}}
		data, err := jsonv1.MarshalIndent(tx, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(data, '\n'), v
	}
	write := func(t *testing.T, v *Vendor, data []byte) {
		t.Helper()
		if err := os.WriteFile(v.transactionPath(), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("well-formed journal recovers", func(t *testing.T) {
		data, v := journal(t)
		write(t, v, data)
		if err := v.recoverTransaction(); err != nil {
			t.Fatalf("recover: %v", err)
		}
	})
	t.Run("trailing whitespace accepted", func(t *testing.T) {
		data, v := journal(t)
		write(t, v, append(data, ' ', '\n', '\t'))
		if err := v.recoverTransaction(); err != nil {
			t.Fatalf("recover: %v", err)
		}
	})
	t.Run("unknown member rejected", func(t *testing.T) {
		data, v := journal(t)
		bad := strings.Replace(string(data), `"version": 1`, `"version": 1,
  "unknown_member": 1`, 1)
		write(t, v, []byte(bad))
		if err := v.recoverTransaction(); err == nil || !strings.Contains(err.Error(), "decode transaction journal") {
			t.Fatalf("unknown member = %v, want a decode error", err)
		}
	})
	t.Run("second value rejected", func(t *testing.T) {
		data, v := journal(t)
		write(t, v, append(data, '{', '}'))
		if err := v.recoverTransaction(); err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
			t.Fatalf("second value = %v, want the multiple-values error", err)
		}
	})
	t.Run("malformed trailing rejected", func(t *testing.T) {
		data, v := journal(t)
		write(t, v, append(data, '{'))
		if err := v.recoverTransaction(); err == nil {
			t.Fatal("malformed trailing input accepted")
		}
	})
}
