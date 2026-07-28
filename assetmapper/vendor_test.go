package assetmapper_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gofabrik/fabrik/assetmapper"
)

// stubResolver returns fixed resolutions and file contents. Tests
// configure it before each scenario. Fetch is concurrency-safe so
// tests still pass with parallel downloads in applyResolution.
type stubResolver struct {
	resolution *assetmapper.Resolution
	fetched    map[string][]byte // url -> content
	onFetch    func()

	mu    sync.Mutex
	calls []string // urls fetched, for ordering / counting
	reqs  [][]assetmapper.PackageRequest
}

type contextResolver struct{}

func (contextResolver) Resolve(ctx context.Context, _ []assetmapper.PackageRequest) (*assetmapper.Resolution, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (contextResolver) Fetch(context.Context, string) ([]byte, error) {
	return nil, errors.New("unexpected fetch")
}

func localJSPMResolver(server *httptest.Server) *assetmapper.JSPMResolver {
	resolver := assetmapper.NewJSPMResolver(server.Client())
	resolver.AllowHTTP = true
	resolver.AllowPrivateNetwork = true
	return resolver
}

func (s *stubResolver) Resolve(ctx context.Context, reqs []assetmapper.PackageRequest) (*assetmapper.Resolution, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, append([]assetmapper.PackageRequest(nil), reqs...))
	s.mu.Unlock()
	return s.resolution, nil
}

func (s *stubResolver) Fetch(ctx context.Context, url string) ([]byte, error) {
	if s.onFetch != nil {
		s.onFetch()
	}
	s.mu.Lock()
	s.calls = append(s.calls, url)
	s.mu.Unlock()
	return s.fetched[url], nil
}

func vendoredPath(t *testing.T, vendorDir string, im *assetmapper.Importmap, specifier string) string {
	t.Helper()
	entry := im.Entries[specifier]
	if entry.Path == "" {
		ext := ".js"
		if entry.Type == "css" {
			ext = ".css"
		}
		return filepath.Join(vendorDir, filepath.FromSlash(specifier+ext))
	}
	const prefix = assetmapper.VendorDir + "/"
	if !strings.HasPrefix(entry.Path, prefix) {
		t.Fatalf("vendored path %q is outside %s", entry.Path, prefix)
	}
	return filepath.Join(vendorDir, filepath.FromSlash(strings.TrimPrefix(entry.Path, prefix)))
}

func TestVendor_MetadataFailureLeavesImportmapUnchanged(t *testing.T) {
	root := t.TempDir()
	vendorDir := filepath.Join(root, "assets", "vendor")
	lockTarget := filepath.Join(root, "assets", "blocked-lock")
	url := "https://example.com/pkg.js"
	resolver := &stubResolver{
		resolution: &assetmapper.Resolution{Packages: []assetmapper.ResolvedPackage{
			{Specifier: "pkg", Version: "1.0.0", Type: "js", URL: url},
		}},
		fetched: map[string][]byte{url: []byte("new bytes")},
		onFetch: func() {
			if err := os.MkdirAll(filepath.Dir(lockTarget), 0o750); err != nil {
				panic(err)
			}
			if err := os.Mkdir(lockTarget, 0o750); err != nil && !os.IsExist(err) {
				panic(err)
			}
		},
	}
	importmap := assetmapper.NewImportmap()
	vendor := &assetmapper.Vendor{
		Resolver:  resolver,
		VendorDir: vendorDir,
		Importmap: importmap,
		Lockfile:  lockTarget,
	}

	err := vendor.Require(context.Background(), "pkg", "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "VendorLock.Save") {
		t.Fatalf("Require error = %v, want lock metadata failure", err)
	}
	if len(importmap.Entries) != 0 {
		t.Fatalf("failed transaction mutated importmap: %v", importmap.Entries)
	}
}

func TestVendor_RecoversInterruptedMetadataCommit(t *testing.T) {
	root := t.TempDir()
	assetsDir := filepath.Join(root, "assets")
	vendorDir := filepath.Join(assetsDir, "vendor")
	importmapPath := filepath.Join(assetsDir, "importmap.json")
	if err := os.MkdirAll(importmapPath, 0o750); err != nil {
		t.Fatal(err)
	}
	url := "https://example.com/pkg.js"
	resolver := &stubResolver{
		resolution: &assetmapper.Resolution{Packages: []assetmapper.ResolvedPackage{
			{Specifier: "pkg", Version: "1.0.0", Type: "js", URL: url},
		}},
		fetched: map[string][]byte{url: []byte("export {}")},
	}
	importmap := assetmapper.NewImportmap()
	vendor := &assetmapper.Vendor{
		Resolver:      resolver,
		VendorDir:     vendorDir,
		Importmap:     importmap,
		ImportmapFile: importmapPath,
	}

	err := vendor.Require(context.Background(), "pkg", "1.0.0")
	if err == nil {
		t.Fatal("Require succeeded despite blocked importmap target")
	}
	if len(importmap.Entries) != 0 {
		t.Fatalf("failed commit mutated in-memory map: %v", importmap.Entries)
	}
	journal := filepath.Join(assetsDir, assetmapper.VendorTransactionFilename)
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("recovery journal missing: %v", err)
	}

	if err := os.Remove(importmapPath); err != nil {
		t.Fatal(err)
	}
	resolver.resolution = &assetmapper.Resolution{}
	if err := vendor.RequirePackages(context.Background(), nil); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if importmap.Entries["pkg"].Version != "1.0.0" {
		t.Fatalf("recovered in-memory map = %+v", importmap.Entries)
	}
	loaded, err := assetmapper.LoadImportmap(importmapPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Entries["pkg"].Path == "" {
		t.Fatalf("recovered persisted map = %+v", loaded.Entries)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("recovery journal survived completed recovery: %v", err)
	}
}

// --- Vendor path safety ---

// Specifier-derived paths cannot escape VendorDir.
func TestVendor_RejectsTraversalSpecifiers(t *testing.T) {
	dir := t.TempDir()
	vendorDir := filepath.Join(dir, "assets", "vendor")

	for _, spec := range []string{"../evil", "/evil", "a/../evil", ".", "", `..\evil`} {
		stub := &stubResolver{
			resolution: &assetmapper.Resolution{
				Packages: []assetmapper.ResolvedPackage{
					{Specifier: spec, Version: "1.0.0", Type: "js", URL: "https://example.com/x.js"},
				},
			},
			fetched: map[string][]byte{"https://example.com/x.js": []byte("export {}")},
		}
		im := assetmapper.NewImportmap()
		v := &assetmapper.Vendor{Resolver: stub, VendorDir: vendorDir, Importmap: im}

		err := v.Require(context.Background(), spec, "")
		want := "safe path"
		if spec == "" {
			want = "empty package name"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Require with specifier %q: err = %v, want safe-path rejection", spec, err)
		}
		if len(im.Entries) != 0 {
			t.Fatalf("Require with specifier %q mutated the importmap", spec)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "assets", "evil.js")); !os.IsNotExist(err) {
		t.Fatal("a traversal specifier escaped the vendor directory")
	}

	// Hand-edited importmap keys are also untrusted.
	im := assetmapper.NewImportmap()
	im.Entries["../evil"] = assetmapper.ImportmapEntry{Version: "1.0.0"}
	v := &assetmapper.Vendor{Resolver: &stubResolver{}, VendorDir: vendorDir, Importmap: im}
	if err := v.Remove("../evil"); err == nil || !strings.Contains(err.Error(), "safe path") {
		t.Fatalf("Remove with traversal key: err = %v, want safe-path rejection", err)
	}
}

func TestVendor_PruneRejectsMalformedVendoredPathBeforeDeleting(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		entry assetmapper.ImportmapEntry
	}{
		{
			name: "immutable path",
			key:  "bad",
			entry: assetmapper.ImportmapEntry{
				Path: "vendor/../keep.js", Version: "1.0.0", Type: "js",
			},
		},
		{
			name:  "legacy specifier",
			key:   "../keep",
			entry: assetmapper.ImportmapEntry{Version: "1.0.0", Type: "js"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			vendorDir := filepath.Join(root, "assets", "vendor")
			if err := os.MkdirAll(vendorDir, 0o750); err != nil {
				t.Fatal(err)
			}
			keep := filepath.Join(vendorDir, "keep.js")
			if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			importmap := assetmapper.NewImportmap()
			importmap.Entries[test.key] = test.entry
			vendor := &assetmapper.Vendor{
				Resolver:  &stubResolver{},
				VendorDir: vendorDir,
				Importmap: importmap,
			}

			if _, err := vendor.Prune(); err == nil {
				t.Fatal("Prune accepted malformed vendored path")
			}
			if got, err := os.ReadFile(keep); err != nil || string(got) != "keep" {
				t.Fatalf("Prune touched keep.js: %q, %v", got, err)
			}
		})
	}
}

func TestVendor_RejectsCollidingOrPublicMetadataPaths(t *testing.T) {
	root := t.TempDir()
	vendorDir := filepath.Join(root, "assets", "vendor")
	importmapPath := filepath.Join(root, "assets", "state.json")
	for _, vendor := range []*assetmapper.Vendor{
		{
			Resolver: &stubResolver{}, VendorDir: vendorDir, Importmap: assetmapper.NewImportmap(),
			Lockfile: importmapPath, ImportmapFile: importmapPath,
		},
		{
			Resolver: &stubResolver{}, VendorDir: vendorDir, Importmap: assetmapper.NewImportmap(),
			Lockfile: filepath.Join(vendorDir, "lock.json"),
		},
	} {
		if err := vendor.Require(context.Background(), "pkg", "1.0.0"); err == nil {
			t.Fatal("Require accepted unsafe metadata paths")
		}
	}

	if err := os.MkdirAll(vendorDir, 0o750); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "assets", "metadata-link")
	if err := os.Symlink(vendorDir, symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	vendor := &assetmapper.Vendor{
		Resolver:  &stubResolver{},
		VendorDir: vendorDir,
		Importmap: assetmapper.NewImportmap(),
		Lockfile:  filepath.Join(symlink, "lock.json"),
	}
	if err := vendor.Require(context.Background(), "pkg", "1.0.0"); err == nil {
		t.Fatal("Require accepted a metadata path symlinked into VendorDir")
	}
}

// --- Vendor.Require ---

func TestVendor_RequireWritesFileAndImportmapEntry(t *testing.T) {
	dir := t.TempDir()
	vendorDir := filepath.Join(dir, "assets", "vendor")

	stub := &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{
					Specifier: "react", Version: "18.2.0", Type: "js",
					URL: "https://example.com/npm:react@18.2.0/index.js",
				},
			},
		},
		fetched: map[string][]byte{
			"https://example.com/npm:react@18.2.0/index.js": []byte(`export default {}`),
		},
	}

	im := assetmapper.NewImportmap()
	v := &assetmapper.Vendor{Resolver: stub, VendorDir: vendorDir, Importmap: im}
	if err := v.Require(context.Background(), "react", "18.2.0"); err != nil {
		t.Fatal(err)
	}

	// File on disk at vendor/react.js.
	data, err := os.ReadFile(vendoredPath(t, vendorDir, im, "react")) // #nosec G304 -- reads an app-selected asset path
	if err != nil {
		t.Fatalf("vendored file missing: %v", err)
	}
	if string(data) != "export default {}" {
		t.Errorf("file content = %q, want %q", data, "export default {}")
	}

	// Importmap entry exists and is shaped right.
	entry, ok := im.Entries["react"]
	if !ok {
		t.Fatal("importmap missing 'react' entry")
	}
	if entry.Version != "18.2.0" {
		t.Errorf("entry Version = %q, want 18.2.0", entry.Version)
	}
	if !strings.HasPrefix(entry.Path, "vendor/react-") || !strings.HasSuffix(entry.Path, ".js") {
		t.Errorf("entry Path = %q, want immutable vendored path", entry.Path)
	}
	lock, err := assetmapper.LoadVendorLock(filepath.Join(filepath.Dir(vendorDir), assetmapper.VendorLockFilename))
	if err != nil {
		t.Fatal(err)
	}
	locked := lock.Packages["react"]
	if locked.Version != "18.2.0" || locked.Type != "js" ||
		locked.SourceURL != "https://example.com/npm:react@18.2.0/index.js" ||
		locked.Size != int64(len("export default {}")) || locked.SHA256 == "" {
		t.Fatalf("locked provenance = %+v", locked)
	}
	if err := lock.Verify(vendorDir); err != nil {
		t.Fatalf("verify lock: %v", err)
	}
	if err := os.WriteFile(vendoredPath(t, vendorDir, im, "react"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lock.Verify(vendorDir); err == nil {
		t.Fatal("Verify accepted tampered vendored bytes")
	}
}

func TestVendor_EnforcesResolutionLimits(t *testing.T) {
	tests := []struct {
		name        string
		packages    []assetmapper.ResolvedPackage
		fetched     map[string][]byte
		limits      assetmapper.DownloadLimits
		want        string
		wantFetches int
	}{
		{
			name: "package count",
			packages: []assetmapper.ResolvedPackage{
				{Specifier: "a", Version: "1.0.0", Type: "js", URL: "https://example.com/a.js"},
				{Specifier: "b", Version: "1.0.0", Type: "js", URL: "https://example.com/b.js"},
			},
			limits:      assetmapper.DownloadLimits{MaxPackages: 1},
			want:        "limit is 1",
			wantFetches: 0,
		},
		{
			name: "package bytes",
			packages: []assetmapper.ResolvedPackage{
				{Specifier: "a", Version: "1.0.0", Type: "js", URL: "https://example.com/a.js"},
			},
			fetched:     map[string][]byte{"https://example.com/a.js": []byte("too large")},
			limits:      assetmapper.DownloadLimits{MaxPackageBytes: 4},
			want:        "4-byte limit",
			wantFetches: 1,
		},
		{
			name: "resolution bytes",
			packages: []assetmapper.ResolvedPackage{
				{Specifier: "a", Version: "1.0.0", Type: "js", URL: "https://example.com/a.js"},
				{Specifier: "b", Version: "1.0.0", Type: "js", URL: "https://example.com/b.js"},
			},
			fetched: map[string][]byte{
				"https://example.com/a.js": []byte("1234"),
				"https://example.com/b.js": []byte("5678"),
			},
			limits:      assetmapper.DownloadLimits{MaxResolutionBytes: 7},
			want:        "7-byte limit",
			wantFetches: 2,
		},
		{
			name: "published package bytes after rewrite",
			packages: []assetmapper.ResolvedPackage{
				{Specifier: "a", Version: "1.0.0", Type: "js", URL: "a"},
				{Specifier: strings.Repeat("dependency", 10), Version: "1.0.0", Type: "js", URL: "b"},
			},
			fetched: map[string][]byte{
				"a": []byte(`import "b"`),
				"b": []byte(`export {}`),
			},
			limits:      assetmapper.DownloadLimits{MaxPackageBytes: 80},
			want:        "published package exceeds 80-byte limit",
			wantFetches: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := &stubResolver{
				resolution: &assetmapper.Resolution{Packages: test.packages},
				fetched:    test.fetched,
			}
			vendorDir := filepath.Join(t.TempDir(), "assets", "vendor")
			vendor := &assetmapper.Vendor{
				Resolver:  resolver,
				VendorDir: vendorDir,
				Importmap: assetmapper.NewImportmap(),
				Limits:    test.limits,
			}
			err := vendor.Require(context.Background(), "a", "")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Require error = %v, want %q", err, test.want)
			}
			if len(resolver.calls) != test.wantFetches {
				t.Fatalf("fetch calls = %d, want %d", len(resolver.calls), test.wantFetches)
			}
			if _, err := os.Stat(vendorDir); !os.IsNotExist(err) {
				t.Fatalf("limit failure published vendor directory: %v", err)
			}
		})
	}
}

func TestVendor_RejectsPartialProvenanceLock(t *testing.T) {
	vendorDir := filepath.Join(t.TempDir(), "assets", "vendor")
	if err := os.MkdirAll(filepath.Dir(vendorDir), 0o750); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(filepath.Dir(vendorDir), assetmapper.VendorLockFilename)
	if err := os.WriteFile(lockPath, []byte("{\"version\":1,\"packages\":{}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolver := &stubResolver{
		resolution: &assetmapper.Resolution{Packages: []assetmapper.ResolvedPackage{
			{Specifier: "new", Version: "1.0.0", Type: "js", URL: "https://example.com/new.js"},
		}},
		fetched: map[string][]byte{"https://example.com/new.js": []byte("export {}")},
	}
	importmap := assetmapper.NewImportmap()
	importmap.Entries["existing"] = assetmapper.ImportmapEntry{Version: "1.0.0", Type: "js"}
	vendor := &assetmapper.Vendor{
		Resolver:  resolver,
		VendorDir: vendorDir,
		Importmap: importmap,
	}

	err := vendor.Require(context.Background(), "new", "1.0.0")
	if err == nil || !strings.Contains(err.Error(), `direct requirement "existing" is absent from resolved closure`) {
		t.Fatalf("Require error = %v, want complete-closure rejection", err)
	}
	if len(resolver.calls) != 0 {
		t.Fatalf("fetch calls = %d, want 0", len(resolver.calls))
	}
	if _, err := os.Stat(vendorDir); !os.IsNotExist(err) {
		t.Fatalf("Require mutated vendor directory: %v", err)
	}
	if _, exists := importmap.Entries["new"]; exists {
		t.Fatal("Require mutated importmap")
	}
}

func TestVendor_RejectsResolverThatDropsExistingDirectRequirement(t *testing.T) {
	vendorDir := filepath.Join(t.TempDir(), "assets", "vendor")
	existingURL := "https://example.com/existing.js"
	resolver := &stubResolver{
		resolution: &assetmapper.Resolution{Packages: []assetmapper.ResolvedPackage{
			{Specifier: "existing", Version: "1.0.0", Type: "js", URL: existingURL},
		}},
		fetched: map[string][]byte{existingURL: []byte("export {}")},
	}
	importmap := assetmapper.NewImportmap()
	vendor := &assetmapper.Vendor{
		Resolver:  resolver,
		VendorDir: vendorDir,
		Importmap: importmap,
	}
	if err := vendor.Require(context.Background(), "existing", "1.0.0"); err != nil {
		t.Fatal(err)
	}

	importmap.Entries["existing"] = assetmapper.ImportmapEntry{Version: "2.0.0", Type: "js"}
	resolver.resolution = &assetmapper.Resolution{Packages: []assetmapper.ResolvedPackage{
		{Specifier: "new", Version: "1.0.0", Type: "js", URL: "https://example.com/new.js"},
	}}
	resolver.fetched = map[string][]byte{"https://example.com/new.js": []byte("export {}")}
	resolver.calls = nil

	err := vendor.Require(context.Background(), "new", "1.0.0")
	if err == nil || !strings.Contains(err.Error(), `direct requirement "existing" is absent from resolved closure`) {
		t.Fatalf("Require error = %v, want complete-closure rejection", err)
	}
	if len(resolver.calls) != 0 {
		t.Fatalf("fetch calls = %d, want 0", len(resolver.calls))
	}
}

func TestVendor_RequireDownloadsTransitiveDeps(t *testing.T) {
	dir := t.TempDir()
	vendorDir := filepath.Join(dir, "assets", "vendor")

	reactURL := "https://example.com/npm:react@18.2.0/index.js"
	schedulerURL := "https://example.com/npm:scheduler@0.23.0/index.js"
	stub := &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.2.0", Type: "js", URL: reactURL},
				{Specifier: "scheduler", Version: "0.23.0", Type: "js", URL: schedulerURL},
			},
		},
		fetched: map[string][]byte{
			reactURL:     []byte(`import sched from "` + schedulerURL + `";`),
			schedulerURL: []byte(`export default {}`),
		},
	}

	im := assetmapper.NewImportmap()
	v := &assetmapper.Vendor{Resolver: stub, VendorDir: vendorDir, Importmap: im}
	if err := v.Require(context.Background(), "react", "18.2.0"); err != nil {
		t.Fatal(err)
	}

	// Both files exist on disk.
	for _, specifier := range []string{"react", "scheduler"} {
		if _, err := os.Stat(vendoredPath(t, vendorDir, im, specifier)); err != nil {
			t.Errorf("missing %s: %v", specifier, err)
		}
	}
	// Both importmap entries exist.
	for _, spec := range []string{"react", "scheduler"} {
		if _, ok := im.Entries[spec]; !ok {
			t.Errorf("missing importmap entry %q", spec)
		}
	}
}

func TestVendor_ReplacesCompleteGraphAndTracksOwnership(t *testing.T) {
	root := t.TempDir()
	vendorDir := filepath.Join(root, "assets", "vendor")
	aURL := "https://example.com/a.js"
	bURL := "https://example.com/b.js"
	sharedV1URL := "https://example.com/shared-v1.js"
	sharedV2URL := "https://example.com/shared-v2.js"
	resolver := &stubResolver{
		resolution: &assetmapper.Resolution{Packages: []assetmapper.ResolvedPackage{
			{Specifier: "a", Version: "1.0.0", Type: "js", URL: aURL},
			{Specifier: "shared", Version: "1.0.0", Type: "js", URL: sharedV1URL},
		}},
		fetched: map[string][]byte{
			aURL:        []byte(`import "` + sharedV1URL + `"`),
			sharedV1URL: []byte(`export {}`),
		},
	}
	importmap := assetmapper.NewImportmap()
	vendor := &assetmapper.Vendor{Resolver: resolver, VendorDir: vendorDir, Importmap: importmap}
	if err := vendor.Require(context.Background(), "a", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(filepath.Dir(vendorDir), assetmapper.VendorLockFilename)
	lock, err := assetmapper.LoadVendorLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if lock.DirectRequirements["a"] != "1.0.0" ||
		!slices.Equal(lock.Packages["a"].Dependencies, []string{"shared"}) ||
		!slices.Equal(lock.Packages["shared"].Owners, []string{"a"}) {
		t.Fatalf("initial graph lock = %+v", lock)
	}
	invalid := *lock
	invalid.DirectRequirements = map[string]string{}
	if err := invalid.Save(filepath.Join(root, "invalid.lock.json")); err == nil {
		t.Fatal("lock accepted a non-empty closure with no direct requirements")
	}
	legacy := *lock
	legacy.Version = 1
	legacy.DirectRequirements = nil
	legacy.Packages = maps.Clone(lock.Packages)
	for specifier, pkg := range legacy.Packages {
		pkg.Dependencies = nil
		pkg.Owners = nil
		legacy.Packages[specifier] = pkg
	}
	legacyBytes, err := json.Marshal(&legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(root, "legacy.lock.json")
	if err := os.WriteFile(legacyPath, legacyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	migrated, err := assetmapper.LoadVendorLock(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Version != 2 || len(migrated.DirectRequirements) != len(migrated.Packages) {
		t.Fatalf("migrated v1 lock = %+v", migrated)
	}

	resolver.resolution = &assetmapper.Resolution{Packages: []assetmapper.ResolvedPackage{
		{Specifier: "a", Version: "1.0.0", Type: "js", URL: aURL},
		{Specifier: "b", Version: "1.0.0", Type: "js", URL: bURL},
		{Specifier: "shared", Version: "2.0.0", Type: "js", URL: sharedV2URL},
	}}
	resolver.fetched = map[string][]byte{
		aURL:        []byte(`import "` + sharedV2URL + `"`),
		bURL:        []byte(`import "shared"`),
		sharedV2URL: []byte(`export {}`),
	}
	if err := vendor.Require(context.Background(), "b", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	lastRequests := resolver.reqs[len(resolver.reqs)-1]
	if !slices.Equal(lastRequests, []assetmapper.PackageRequest{
		{Name: "a", Version: "1.0.0"},
		{Name: "b", Version: "1.0.0"},
	}) {
		t.Fatalf("second resolve requests = %+v", lastRequests)
	}
	lock, err = assetmapper.LoadVendorLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Packages["shared"].Version != "2.0.0" ||
		!slices.Equal(lock.Packages["shared"].Owners, []string{"a", "b"}) {
		t.Fatalf("updated shared lock = %+v", lock.Packages["shared"])
	}

	resolver.resolution = &assetmapper.Resolution{Packages: []assetmapper.ResolvedPackage{
		{Specifier: "b", Version: "1.0.0", Type: "js", URL: bURL},
		{Specifier: "shared", Version: "2.0.0", Type: "js", URL: sharedV2URL},
	}}
	if err := vendor.Remove("a"); err != nil {
		t.Fatal(err)
	}
	lock, err = assetmapper.LoadVendorLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lock.Packages["a"]; ok {
		t.Fatal("removed direct package remains in complete closure")
	}
	if !slices.Equal(lock.Packages["shared"].Owners, []string{"b"}) {
		t.Fatalf("shared owners after remove = %v", lock.Packages["shared"].Owners)
	}
	if _, ok := importmap.Entries["a"]; ok {
		t.Fatal("removed direct package remains in importmap")
	}

	resolver.resolution = &assetmapper.Resolution{}
	if err := vendor.Remove("b"); err != nil {
		t.Fatal(err)
	}
	lock, err = assetmapper.LoadVendorLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.DirectRequirements) != 0 || len(lock.Packages) != 0 {
		t.Fatalf("empty graph lock = %+v", lock)
	}
}

func TestVendor_RequireRewritesUpstreamURLToBareSpecifier(t *testing.T) {
	// React's downloaded source imports scheduler via the upstream
	// URL; the rewriter must convert that to a bare specifier so the
	// browser resolves it through our importmap (pointing at the
	// local vendored scheduler.js).
	dir := t.TempDir()
	vendorDir := filepath.Join(dir, "assets", "vendor")

	reactURL := "https://example.com/npm:react@18.2.0/index.js"
	schedulerURL := "https://example.com/npm:scheduler@0.23.0/index.js"
	stub := &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.2.0", Type: "js", URL: reactURL},
				{Specifier: "scheduler", Version: "0.23.0", Type: "js", URL: schedulerURL},
			},
		},
		fetched: map[string][]byte{
			reactURL:     []byte(`import sched from "` + schedulerURL + `"; export default sched;`),
			schedulerURL: []byte(`export default {}`),
		},
	}

	im := assetmapper.NewImportmap()
	v := &assetmapper.Vendor{Resolver: stub, VendorDir: vendorDir, Importmap: im}
	if err := v.Require(context.Background(), "react", "18.2.0"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(vendoredPath(t, vendorDir, im, "react")) // #nosec G304 -- reads an app-selected asset path
	got := string(data)
	if !strings.Contains(got, `"scheduler"`) {
		t.Errorf("upstream URL not rewritten to bare specifier; got:\n%s", got)
	}
	if strings.Contains(got, schedulerURL) {
		t.Errorf("upstream URL still present after rewrite; got:\n%s", got)
	}
}

func TestVendor_RequireSupportsCSSPackages(t *testing.T) {
	dir := t.TempDir()
	vendorDir := filepath.Join(dir, "assets", "vendor")

	stub := &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{
					Specifier: "normalize", Version: "8.0.1", Type: "css",
					URL: "https://example.com/npm:normalize@8.0.1/normalize.css",
				},
			},
		},
		fetched: map[string][]byte{
			"https://example.com/npm:normalize@8.0.1/normalize.css": []byte(`*{margin:0}`),
		},
	}

	im := assetmapper.NewImportmap()
	v := &assetmapper.Vendor{Resolver: stub, VendorDir: vendorDir, Importmap: im}
	if err := v.Require(context.Background(), "normalize", "8.0.1"); err != nil {
		t.Fatal(err)
	}

	// File at vendor/normalize.css (not .js).
	if _, err := os.Stat(vendoredPath(t, vendorDir, im, "normalize")); err != nil {
		t.Errorf("missing CSS file: %v", err)
	}
	entry := im.Entries["normalize"]
	if entry.Type != "css" {
		t.Errorf("entry Type = %q, want css", entry.Type)
	}
}

func TestVendor_RequireOverwritesExistingEntry(t *testing.T) {
	// Require doubles as an update operation.
	dir := t.TempDir()
	vendorDir := filepath.Join(dir, "assets", "vendor")
	v1URL := "https://example.com/npm:react@18.0.0/index.js"
	v2URL := "https://example.com/npm:react@18.2.0/index.js"

	stub := &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{Specifier: "react", Version: "18.0.0", Type: "js", URL: v1URL},
			},
		},
		fetched: map[string][]byte{v1URL: []byte("v1")},
	}
	im := assetmapper.NewImportmap()
	v := &assetmapper.Vendor{Resolver: stub, VendorDir: vendorDir, Importmap: im}
	if err := v.Require(context.Background(), "react", "18.0.0"); err != nil {
		t.Fatal(err)
	}

	// Now require a different version.
	stub.resolution = &assetmapper.Resolution{
		Packages: []assetmapper.ResolvedPackage{
			{Specifier: "react", Version: "18.2.0", Type: "js", URL: v2URL},
		},
	}
	stub.fetched = map[string][]byte{v2URL: []byte("v2")}
	if err := v.Require(context.Background(), "react", "18.2.0"); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(vendoredPath(t, vendorDir, im, "react")) // #nosec G304 -- reads an app-selected asset path
	if string(data) != "v2" {
		t.Errorf("file content = %q, want v2 (overwrite)", data)
	}
	if im.Entries["react"].Version != "18.2.0" {
		t.Errorf("entry Version = %q, want 18.2.0", im.Entries["react"].Version)
	}
}

// --- Vendor.Remove ---

func TestVendor_RemoveUnregistersFileUntilPrune(t *testing.T) {
	dir := t.TempDir()
	vendorDir := filepath.Join(dir, "assets", "vendor")

	stub := &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{
					Specifier: "react", Version: "18.2.0", Type: "js",
					URL: "https://example.com/npm:react@18.2.0/index.js",
				},
			},
		},
		fetched: map[string][]byte{
			"https://example.com/npm:react@18.2.0/index.js": []byte("x"),
		},
	}
	im := assetmapper.NewImportmap()
	v := &assetmapper.Vendor{Resolver: stub, VendorDir: vendorDir, Importmap: im}
	if err := v.Require(context.Background(), "react", "18.2.0"); err != nil {
		t.Fatal(err)
	}

	artifact := vendoredPath(t, vendorDir, im, "react")
	if err := v.Remove("react"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact); err != nil {
		t.Errorf("immutable file should remain until Prune: %v", err)
	}
	if _, ok := im.Entries["react"]; ok {
		t.Error("entry still present in importmap after Remove")
	}
	if _, err := v.Prune(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Errorf("file survived Prune: %v", err)
	}
}

func TestVendor_RemoveContextCancelsFullResolution(t *testing.T) {
	root := t.TempDir()
	vendorDir := filepath.Join(root, "assets", "vendor")
	url := "https://example.com/pkg.js"
	otherURL := "https://example.com/other.js"
	importmap := assetmapper.NewImportmap()
	vendor := &assetmapper.Vendor{
		Resolver: &stubResolver{
			resolution: &assetmapper.Resolution{Packages: []assetmapper.ResolvedPackage{
				{Specifier: "pkg", Version: "1.0.0", Type: "js", URL: url},
				{Specifier: "other", Version: "1.0.0", Type: "js", URL: otherURL},
			}},
			fetched: map[string][]byte{
				url:      []byte("export {}"),
				otherURL: []byte("export {}"),
			},
		},
		VendorDir: vendorDir,
		Importmap: importmap,
	}
	if err := vendor.RequirePackages(context.Background(), []assetmapper.PackageRequest{
		{Name: "pkg", Version: "1.0.0"},
		{Name: "other", Version: "1.0.0"},
	}); err != nil {
		t.Fatal(err)
	}
	vendor.Resolver = contextResolver{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := vendor.RemoveContext(ctx, "pkg")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RemoveContext error = %v, want context cancellation", err)
	}
	if _, ok := importmap.Entries["pkg"]; !ok {
		t.Fatal("cancelled removal mutated importmap")
	}
}

func TestVendor_RemoveRefusesLocalEntry(t *testing.T) {
	// Local entries belong to the user, not vendoring.
	dir := t.TempDir()
	vendorDir := filepath.Join(dir, "assets", "vendor")

	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}
	v := &assetmapper.Vendor{
		Resolver: &stubResolver{}, VendorDir: vendorDir, Importmap: im,
	}
	if err := v.Remove("app"); err == nil {
		t.Fatal("expected error removing local entry")
	}
	if _, ok := im.Entries["app"]; !ok {
		t.Error("local entry was deleted")
	}
}

func TestVendor_RemoveMissingEntryIsError(t *testing.T) {
	v := &assetmapper.Vendor{
		Resolver: &stubResolver{}, VendorDir: t.TempDir(),
		Importmap: assetmapper.NewImportmap(),
	}
	if err := v.Remove("nope"); err == nil {
		t.Fatal("expected error removing absent entry")
	}
}

// --- JSPMResolver against an httptest.Server ---

// Conflicting scoped JSPM resolutions are rejected before flattening.
func TestJSPMResolver_RejectsConflictingScopes(t *testing.T) {
	response := `{"map": {"imports": {}, "scopes": {}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	defer srv.Close()
	res := localJSPMResolver(srv)
	res.BaseURL = srv.URL

	response = `{"map": {"scopes": {
		"https://a/": {"scheduler": "https://ga.jspm.io/npm:scheduler@0.23.0/index.js"},
		"https://b/": {"scheduler": "https://ga.jspm.io/npm:scheduler@0.26.0/index.js"}
	}}}`
	_, err := res.Resolve(context.Background(), []assetmapper.PackageRequest{{Name: "react"}})
	if err == nil || !strings.Contains(err.Error(), `specifier "scheduler" resolves to both`) {
		t.Fatalf("conflicting scopes: err = %v, want conflict rejection", err)
	}

	response = `{"map": {"scopes": {
		"https://a/": {"scheduler": "https://ga.jspm.io/npm:scheduler@0.23.0/index.js"},
		"https://b/": {"scheduler": "https://ga.jspm.io/npm:scheduler@0.23.0/index.js"}
	}}}`
	got, err := res.Resolve(context.Background(), []assetmapper.PackageRequest{{Name: "react"}})
	if err != nil {
		t.Fatalf("identical duplicate: %v", err)
	}
	if len(got.Packages) != 1 {
		t.Fatalf("identical duplicate should deduplicate; got %d packages", len(got.Packages))
	}
}

func TestJSPMResolver_ResolvesViaGenerateEndpoint(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"map": {
				"imports": {
					"react": "https://ga.jspm.io/npm:react@18.2.0/index.js"
				},
				"scopes": {
					"https://ga.jspm.io/": {
						"scheduler": "https://ga.jspm.io/npm:scheduler@0.23.0/index.js"
					}
				}
			}
		}`))
	}))
	defer srv.Close()

	res := localJSPMResolver(srv)
	res.BaseURL = srv.URL
	got, err := res.Resolve(context.Background(), []assetmapper.PackageRequest{
		{Name: "react", Version: "18.2.0"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPost || gotPath != "/generate" {
		t.Errorf("server saw %s %s, want POST /generate", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"react@18.2.0"`) {
		t.Errorf("request body missing install entry; got: %s", gotBody)
	}

	// Flat resolution should include both imports + scoped transitive deps.
	specs := map[string]assetmapper.ResolvedPackage{}
	for _, p := range got.Packages {
		specs[p.Specifier] = p
	}
	if _, ok := specs["react"]; !ok {
		t.Error("missing react in flattened resolution")
	}
	if _, ok := specs["scheduler"]; !ok {
		t.Error("missing scheduler (transitive) in flattened resolution")
	}
	if specs["react"].Version != "18.2.0" {
		t.Errorf("react version = %q, want 18.2.0", specs["react"].Version)
	}
	if specs["scheduler"].Version != "0.23.0" {
		t.Errorf("scheduler version = %q, want 0.23.0", specs["scheduler"].Version)
	}
}

func TestJSPMResolver_ResolvesScopedPackageVersion(t *testing.T) {
	// "@radix-ui/themes" has an "@" in its name; the version parser
	// must use the LAST "@" before the path component, not the first.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"map": {
				"imports": {
					"@radix-ui/themes": "https://ga.jspm.io/npm:@radix-ui/themes@1.2.3/index.js"
				}
			}
		}`))
	}))
	defer srv.Close()

	res := localJSPMResolver(srv)
	res.BaseURL = srv.URL
	got, err := res.Resolve(context.Background(), []assetmapper.PackageRequest{
		{Name: "@radix-ui/themes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Packages) != 1 {
		t.Fatalf("len = %d, want 1", len(got.Packages))
	}
	if got.Packages[0].Version != "1.2.3" {
		t.Errorf("version = %q, want 1.2.3", got.Packages[0].Version)
	}
}

func TestJSPMResolver_FetchDownloadsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/npm:react@18.2.0/index.js" {
			_, _ = w.Write([]byte(`export default {}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	res := localJSPMResolver(srv)
	data, err := res.Fetch(context.Background(), srv.URL+"/npm:react@18.2.0/index.js")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "export default {}" {
		t.Errorf("Fetch content = %q, want export default {}", data)
	}
}

func TestJSPMResolver_FetchSurfacesNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	res := localJSPMResolver(srv)
	_, err := res.Fetch(context.Background(), srv.URL+"/x")
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

// --- End-to-end: JSPMResolver + Vendor + Importmap render ---

func TestVendor_EndToEnd_WithJSPMResolver(t *testing.T) {
	dir := t.TempDir()
	assetsDir := filepath.Join(dir, "assets")
	vendorDir := filepath.Join(assetsDir, "vendor")

	reactSrc := `import sched from "https://ga.jspm.io/npm:scheduler@0.23.0/index.js"; export default sched;`
	schedSrc := `export default function(){};`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/generate":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"map": {
					"imports": {
						"react": "` + "https://ga.jspm.io/npm:react@18.2.0/index.js" + `"
					},
					"scopes": {
						"https://ga.jspm.io/": {
							"scheduler": "` + "https://ga.jspm.io/npm:scheduler@0.23.0/index.js" + `"
						}
					}
				}
			}`))
		case strings.HasSuffix(r.URL.Path, "npm:react@18.2.0/index.js"):
			_, _ = w.Write([]byte(reactSrc))
		case strings.HasSuffix(r.URL.Path, "npm:scheduler@0.23.0/index.js"):
			_, _ = w.Write([]byte(schedSrc))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	res := localJSPMResolver(srv)
	res.BaseURL = srv.URL

	// Fetch redirects absolute JSPM URLs to the test server.
	wrapped := &fetchRedirectResolver{
		Inner:   res,
		Prefix:  "https://ga.jspm.io",
		Replace: srv.URL,
	}

	im := assetmapper.NewImportmap()
	v := &assetmapper.Vendor{Resolver: wrapped, VendorDir: vendorDir, Importmap: im}
	if err := v.Require(context.Background(), "react", "18.2.0"); err != nil {
		t.Fatal(err)
	}

	// Both vendored files written.
	for _, name := range []string{"react.js", "scheduler.js"} {
		if _, err := os.Stat(filepath.Join(vendorDir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	// React's content has the upstream URL rewritten to a bare specifier.
	reactOut, _ := os.ReadFile(filepath.Join(vendorDir, "react.js")) // #nosec G304 -- reads an app-selected asset path
	if !strings.Contains(string(reactOut), `"scheduler"`) {
		t.Errorf("react.js not rewritten; got:\n%s", reactOut)
	}
}

type fetchRedirectResolver struct {
	Inner   *assetmapper.JSPMResolver
	Prefix  string
	Replace string
}

func (f *fetchRedirectResolver) Resolve(ctx context.Context, reqs []assetmapper.PackageRequest) (*assetmapper.Resolution, error) {
	return f.Inner.Resolve(ctx, reqs)
}

func (f *fetchRedirectResolver) Fetch(ctx context.Context, url string) ([]byte, error) {
	if strings.HasPrefix(url, f.Prefix) {
		url = f.Replace + strings.TrimPrefix(url, f.Prefix)
	}
	return f.Inner.Fetch(ctx, url)
}

// --- Vendor.Prune ---

func TestVendor_PruneRemovesOrphanedFiles(t *testing.T) {
	tmp := t.TempDir()
	vendorDir := filepath.Join(tmp, "assets", "vendor")
	if err := os.MkdirAll(vendorDir, 0o750); err != nil {
		t.Fatal(err)
	}
	// Three files on disk, only one registered → other two should
	// be deleted.
	for _, name := range []string{"react.js", "scheduler.js", "lodash.js"} {
		if err := os.WriteFile(filepath.Join(vendorDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	im := assetmapper.NewImportmap()
	im.Entries["react"] = assetmapper.ImportmapEntry{Version: "18.2.0"}

	v := &assetmapper.Vendor{
		Resolver: &stubResolver{}, VendorDir: vendorDir, Importmap: im,
	}
	removed, err := v.Prune()
	if err != nil {
		t.Fatal(err)
	}
	wantRemoved := []string{"lodash.js", "scheduler.js"} // sorted
	if !equalStringSlice(removed, wantRemoved) {
		t.Errorf("removed = %v, want %v", removed, wantRemoved)
	}
	if _, err := os.Stat(filepath.Join(vendorDir, "react.js")); err != nil {
		t.Errorf("react.js was deleted but is registered: %v", err)
	}
	for _, name := range []string{"scheduler.js", "lodash.js"} {
		if _, err := os.Stat(filepath.Join(vendorDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still exists after Prune: %v", name, err)
		}
	}
}

func TestVendor_PruneHandlesScopedPackages(t *testing.T) {
	tmp := t.TempDir()
	vendorDir := filepath.Join(tmp, "assets", "vendor")
	scoped := filepath.Join(vendorDir, "@radix-ui")
	if err := os.MkdirAll(scoped, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scoped, "themes.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scoped, "icons.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["@radix-ui/themes"] = assetmapper.ImportmapEntry{Version: "1.0.0"}

	v := &assetmapper.Vendor{
		Resolver: &stubResolver{}, VendorDir: vendorDir, Importmap: im,
	}
	removed, err := v.Prune()
	if err != nil {
		t.Fatal(err)
	}
	if !equalStringSlice(removed, []string{"@radix-ui/icons.js"}) {
		t.Errorf("removed = %v, want [@radix-ui/icons.js]", removed)
	}
	if _, err := os.Stat(filepath.Join(scoped, "themes.js")); err != nil {
		t.Errorf("scoped themes.js wrongly deleted: %v", err)
	}
}

func TestVendor_PruneSkipsLocalEntries(t *testing.T) {
	// A local entry (Path set, no Version) doesn't have a vendor
	// file; Prune must not look for one.
	tmp := t.TempDir()
	vendorDir := filepath.Join(tmp, "assets", "vendor")
	if err := os.MkdirAll(vendorDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vendorDir, "stray.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	im := assetmapper.NewImportmap()
	im.Entries["app"] = assetmapper.ImportmapEntry{Path: "app.js", Entrypoint: true}

	v := &assetmapper.Vendor{
		Resolver: &stubResolver{}, VendorDir: vendorDir, Importmap: im,
	}
	removed, err := v.Prune()
	if err != nil {
		t.Fatal(err)
	}
	if !equalStringSlice(removed, []string{"stray.js"}) {
		t.Errorf("removed = %v, want [stray.js]", removed)
	}
}

func TestVendor_PruneEmptyImportmapRemovesEverything(t *testing.T) {
	tmp := t.TempDir()
	vendorDir := filepath.Join(tmp, "assets", "vendor")
	if err := os.MkdirAll(vendorDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.js", "b.js", "c.js"} {
		if err := os.WriteFile(filepath.Join(vendorDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	v := &assetmapper.Vendor{
		Resolver: &stubResolver{}, VendorDir: vendorDir,
		Importmap: assetmapper.NewImportmap(),
	}
	removed, err := v.Prune()
	if err != nil {
		t.Fatal(err)
	}
	if !equalStringSlice(removed, []string{"a.js", "b.js", "c.js"}) {
		t.Errorf("removed = %v, want all three files", removed)
	}
}

func TestVendor_PruneMissingVendorDirIsNoOp(t *testing.T) {
	tmp := t.TempDir()
	v := &assetmapper.Vendor{
		Resolver:  &stubResolver{},
		VendorDir: filepath.Join(tmp, "does-not-exist"),
		Importmap: assetmapper.NewImportmap(),
	}
	removed, err := v.Prune()
	if err != nil {
		t.Fatalf("Prune on missing VendorDir errored: %v", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want empty", removed)
	}
}

func TestVendor_PruneCleansUpEmptyDirs(t *testing.T) {
	tmp := t.TempDir()
	vendorDir := filepath.Join(tmp, "assets", "vendor")
	nested := filepath.Join(vendorDir, "deep", "nested", "scope")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "orphan.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	v := &assetmapper.Vendor{
		Resolver:  &stubResolver{},
		VendorDir: vendorDir,
		Importmap: assetmapper.NewImportmap(),
	}
	if _, err := v.Prune(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(vendorDir, "deep", "nested", "scope"),
		filepath.Join(vendorDir, "deep", "nested"),
		filepath.Join(vendorDir, "deep"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s to be cleaned up; stat err = %v", p, err)
		}
	}
	// VendorDir itself stays.
	if _, err := os.Stat(vendorDir); err != nil {
		t.Errorf("VendorDir was removed: %v", err)
	}
}

func TestVendor_PruneValidatesConfig(t *testing.T) {
	v := &assetmapper.Vendor{} // nothing set
	if _, err := v.Prune(); err == nil {
		t.Fatal("expected error for unconfigured Vendor")
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] { // #nosec G602 -- slices are length-checked before paired indexing
			return false
		}
	}
	return true
}

// --- partial-failure rollback (memory staging) ---

// failingResolver returns staged content for some URLs and an error
// for others, so tests can simulate a mid-resolution failure.
type failingResolver struct {
	resolution *assetmapper.Resolution
	fetched    map[string][]byte
	failURL    string
	failErr    error
}

func (f *failingResolver) Resolve(ctx context.Context, reqs []assetmapper.PackageRequest) (*assetmapper.Resolution, error) {
	return f.resolution, nil
}

func (f *failingResolver) Fetch(ctx context.Context, url string) ([]byte, error) {
	if url == f.failURL {
		return nil, f.failErr
	}
	if data, ok := f.fetched[url]; ok {
		return data, nil
	}
	return nil, errors.New("unexpected fetch URL: " + url)
}

func TestVendor_RequireFetchFailure_LeavesNoFilesOnDisk(t *testing.T) {
	// A failed download leaves disk and importmap state unchanged.
	tmp := t.TempDir()
	vendorDir := filepath.Join(tmp, "assets", "vendor")
	failURL := "https://example.com/scheduler.js"

	stub := &failingResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{
					Specifier: "react", Version: "18.2.0", Type: "js",
					URL: "https://example.com/react.js",
				},
				{
					Specifier: "scheduler", Version: "0.23.0", Type: "js",
					URL: failURL,
				},
				{
					Specifier: "lodash", Version: "4.17.0", Type: "js",
					URL: "https://example.com/lodash.js",
				},
			},
		},
		fetched: map[string][]byte{
			"https://example.com/react.js":  []byte("//react"),
			"https://example.com/lodash.js": []byte("//lodash"),
		},
		failURL: failURL,
		failErr: errors.New("network down"),
	}

	im := assetmapper.NewImportmap()
	v := &assetmapper.Vendor{Resolver: stub, VendorDir: vendorDir, Importmap: im}
	err := v.Require(context.Background(), "react", "")
	if err == nil {
		t.Fatal("expected error from failing fetch")
	}
	if !strings.Contains(err.Error(), "network down") {
		t.Errorf("error did not propagate: %v", err)
	}

	// Vendor dir must be untouched: either it doesn't exist, or it
	// contains only the dir itself (no files). Even the partially-
	// downloaded react.js and lodash.js must not be on disk.
	for _, name := range []string{"react.js", "scheduler.js", "lodash.js"} {
		if _, err := os.Stat(filepath.Join(vendorDir, name)); !os.IsNotExist(err) {
			t.Errorf("file %s present after failed Require: %v", name, err)
		}
	}

	// Importmap must be unchanged.
	if len(im.Entries) != 0 {
		t.Errorf("importmap mutated after failed Require: %v", im.Entries)
	}
}

func TestVendor_RequireFetchFailure_LeavesPriorEntriesIntact(t *testing.T) {
	// Sanity check the "untouched" invariant when prior state exists:
	// a previous Require populated react@18.0; the new Require for
	// "newpkg" fails; react@18.0 must still be in the importmap.
	tmp := t.TempDir()
	vendorDir := filepath.Join(tmp, "assets", "vendor")

	seed := &stubResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{
					Specifier: "react", Version: "18.0.0", Type: "js",
					URL: "https://example.com/react.js",
				},
			},
		},
		fetched: map[string][]byte{"https://example.com/react.js": []byte("//react")},
	}
	im := assetmapper.NewImportmap()
	v := &assetmapper.Vendor{Resolver: seed, VendorDir: vendorDir, Importmap: im}
	if err := v.Require(context.Background(), "react", "18.0.0"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Swap in a failing resolver for the second Require.
	v.Resolver = &failingResolver{
		resolution: &assetmapper.Resolution{
			Packages: []assetmapper.ResolvedPackage{
				{
					Specifier: "newpkg", Version: "1.0.0", Type: "js",
					URL: "https://example.com/newpkg.js",
				},
			},
		},
		failURL: "https://example.com/newpkg.js",
		failErr: errors.New("404"),
	}
	if err := v.Require(context.Background(), "newpkg", "1.0.0"); err == nil {
		t.Fatal("expected error")
	}

	if im.Entries["react"].Version != "18.0.0" {
		t.Errorf("react entry was clobbered by failed Require: %+v", im.Entries["react"])
	}
	if _, ok := im.Entries["newpkg"]; ok {
		t.Error("newpkg entry present despite failed Require")
	}
	if _, err := os.Stat(vendoredPath(t, vendorDir, im, "react")); err != nil {
		t.Errorf("react.js was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vendorDir, "newpkg.js")); !os.IsNotExist(err) {
		t.Errorf("newpkg.js was written despite failure: %v", err)
	}
}
