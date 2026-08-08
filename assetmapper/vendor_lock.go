package assetmapper

import (
	"crypto/sha256"
	"encoding/hex"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// VendorLockFilename is the provenance record written beside VendorDir.
const VendorLockFilename = "vendor.lock.json"

const vendorLockVersion = 2

// VendorLock records the exact source and published bytes of vendored files.
type VendorLock struct {
	Version            int                      `json:"version"`
	DirectRequirements map[string]string        `json:"direct_requirements"`
	Packages           map[string]LockedPackage `json:"packages"`
}

// LockedPackage records one published vendored artifact.
type LockedPackage struct {
	Version      string   `json:"version"`
	Type         string   `json:"type"`
	Path         string   `json:"path,omitempty"`
	SourceURL    string   `json:"source_url"`
	SourceSize   int64    `json:"source_size"`
	SourceSHA256 string   `json:"source_sha256"`
	Size         int64    `json:"size"`
	SHA256       string   `json:"sha256"`
	Dependencies []string `json:"dependencies,omitempty"`
	Owners       []string `json:"owners,omitempty"`
}

func newVendorLock() *VendorLock {
	return &VendorLock{
		Version:            vendorLockVersion,
		DirectRequirements: map[string]string{},
		Packages:           map[string]LockedPackage{},
	}
}

// LoadVendorLock reads and validates a vendoring provenance lock.
func LoadVendorLock(path string) (*VendorLock, error) {
	file, err := os.Open(path) // #nosec G304 -- reads a caller-selected project lockfile
	if err != nil {
		return nil, fmt.Errorf("assetmapper.LoadVendorLock: open %s: %w", path, err)
	}
	defer file.Close() //nolint:errcheck // read-only lockfile close is cleanup only

	var lock VendorLock
	dec := jsontext.NewDecoder(file)
	if err := json.UnmarshalDecode(dec, &lock, jsonv1.DefaultOptionsV1(), json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("assetmapper.LoadVendorLock: decode %s: %w", path, err)
	}
	if err := json.UnmarshalDecode(dec, &struct{}{}, jsonv1.DefaultOptionsV1()); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("assetmapper.LoadVendorLock: decode %s: multiple JSON values", path)
		}
		return nil, fmt.Errorf("assetmapper.LoadVendorLock: decode %s: %w", path, err)
	}
	if lock.Version != 1 && lock.Version != vendorLockVersion {
		return nil, fmt.Errorf("assetmapper.LoadVendorLock: unsupported version %d", lock.Version)
	}
	if lock.Version == 1 {
		migrateVendorLockV1(&lock)
	}
	if lock.Packages == nil {
		lock.Packages = map[string]LockedPackage{}
	}
	for specifier, pkg := range lock.Packages {
		if _, err := vendorRelPath(specifier, pkg.Type); err != nil {
			return nil, fmt.Errorf("assetmapper.LoadVendorLock: %w", err)
		}
		if err := validateLockedPackage(specifier, pkg); err != nil {
			return nil, fmt.Errorf("assetmapper.LoadVendorLock: %w", err)
		}
	}
	if err := validateVendorGraph(&lock); err != nil {
		return nil, fmt.Errorf("assetmapper.LoadVendorLock: %w", err)
	}
	return &lock, nil
}

// Save writes a deterministic vendoring provenance lock.
func (l *VendorLock) Save(path string) error {
	if l == nil {
		return fmt.Errorf("assetmapper.VendorLock.Save: nil lock")
	}
	if l.Version != vendorLockVersion {
		return fmt.Errorf("assetmapper.VendorLock.Save: unsupported version %d", l.Version)
	}
	for specifier, pkg := range l.Packages {
		if _, err := vendorRelPath(specifier, pkg.Type); err != nil {
			return fmt.Errorf("assetmapper.VendorLock.Save: %w", err)
		}
		if err := validateLockedPackage(specifier, pkg); err != nil {
			return fmt.Errorf("assetmapper.VendorLock.Save: %w", err)
		}
	}
	if err := validateVendorGraph(l); err != nil {
		return fmt.Errorf("assetmapper.VendorLock.Save: %w", err)
	}
	data, err := json.Marshal(l, jsonv1.DefaultOptionsV1(), json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("assetmapper.VendorLock.Save: %w", err)
	}
	data = append(data, '\n')
	if err := atomicWriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("assetmapper.VendorLock.Save: write %s: %w", path, err)
	}
	return nil
}

func validateVendorGraph(lock *VendorLock) error {
	if lock.DirectRequirements == nil {
		return fmt.Errorf("nil direct requirements")
	}
	for specifier, version := range lock.DirectRequirements {
		if version == "" {
			return fmt.Errorf("direct requirement %q has no exact version", specifier)
		}
		if _, err := vendorRelPath(specifier, "js"); err != nil {
			return fmt.Errorf("invalid direct requirement: %w", err)
		}
		if _, ok := lock.Packages[specifier]; !ok {
			return fmt.Errorf("direct requirement %q is absent from the resolved closure", specifier)
		}
		if lock.Packages[specifier].Version != version {
			return fmt.Errorf("direct requirement %q version %s does not match resolved version %s",
				specifier, version, lock.Packages[specifier].Version)
		}
	}
	for specifier, pkg := range lock.Packages {
		for _, dependency := range pkg.Dependencies {
			if _, ok := lock.Packages[dependency]; !ok {
				return fmt.Errorf("package %q depends on missing package %q", specifier, dependency)
			}
		}
		for _, owner := range pkg.Owners {
			if _, ok := lock.DirectRequirements[owner]; !ok {
				return fmt.Errorf("package %q has unknown owner %q", specifier, owner)
			}
		}
		if !sort.StringsAreSorted(pkg.Dependencies) || hasDuplicateStrings(pkg.Dependencies) {
			return fmt.Errorf("package %q dependencies are not sorted and unique", specifier)
		}
		if !sort.StringsAreSorted(pkg.Owners) || hasDuplicateStrings(pkg.Owners) {
			return fmt.Errorf("package %q owners are not sorted and unique", specifier)
		}
	}
	if len(lock.DirectRequirements) == 0 && len(lock.Packages) != 0 {
		return fmt.Errorf("resolved closure is non-empty but has no direct requirements")
	}
	expectedOwners := make(map[string]map[string]struct{}, len(lock.Packages))
	for root := range lock.DirectRequirements {
		seen := map[string]struct{}{}
		var visit func(string)
		visit = func(specifier string) {
			if _, ok := seen[specifier]; ok {
				return
			}
			seen[specifier] = struct{}{}
			if expectedOwners[specifier] == nil {
				expectedOwners[specifier] = map[string]struct{}{}
			}
			expectedOwners[specifier][root] = struct{}{}
			for _, dependency := range lock.Packages[specifier].Dependencies {
				visit(dependency)
			}
		}
		visit(root)
	}
	for specifier, pkg := range lock.Packages {
		var expected []string
		for owner := range expectedOwners[specifier] {
			expected = append(expected, owner)
		}
		sort.Strings(expected)
		if len(expected) == 0 {
			return fmt.Errorf("package %q is not owned by any direct requirement", specifier)
		}
		if !slices.Equal(pkg.Owners, expected) {
			return fmt.Errorf("package %q owners %v do not match graph owners %v", specifier, pkg.Owners, expected)
		}
	}
	return nil
}

func migrateVendorLockV1(lock *VendorLock) {
	lock.Version = vendorLockVersion
	lock.DirectRequirements = make(map[string]string, len(lock.Packages))
	for specifier, pkg := range lock.Packages {
		lock.DirectRequirements[specifier] = pkg.Version
		pkg.Dependencies = nil
		pkg.Owners = []string{specifier}
		lock.Packages[specifier] = pkg
	}
}

func hasDuplicateStrings(values []string) bool {
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			return true
		}
	}
	return false
}

// Verify checks every locked artifact against its recorded size and digest.
func (l *VendorLock) Verify(vendorDir string) error {
	if l == nil {
		return fmt.Errorf("assetmapper.VendorLock.Verify: nil lock")
	}
	if l.Version != vendorLockVersion {
		return fmt.Errorf("assetmapper.VendorLock.Verify: unsupported version %d", l.Version)
	}
	for specifier, pkg := range l.Packages {
		if _, err := vendorRelPath(specifier, pkg.Type); err != nil {
			return fmt.Errorf("assetmapper.VendorLock.Verify: %w", err)
		}
		if err := validateLockedPackage(specifier, pkg); err != nil {
			return fmt.Errorf("assetmapper.VendorLock.Verify: %w", err)
		}
	}
	if err := validateVendorGraph(l); err != nil {
		return fmt.Errorf("assetmapper.VendorLock.Verify: %w", err)
	}
	for specifier, pkg := range l.Packages {
		rel := pkg.Path
		if rel == "" {
			var err error
			rel, err = vendorRelPath(specifier, pkg.Type)
			if err != nil {
				return err
			}
		}
		path := filepath.Join(vendorDir, filepath.FromSlash(rel))
		file, err := os.Open(path) // #nosec G304 -- path is validated beneath the caller-selected vendor directory
		if err != nil {
			return fmt.Errorf("assetmapper.VendorLock.Verify: open %s: %w", path, err)
		}
		hash := sha256.New()
		size, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("assetmapper.VendorLock.Verify: read %s: %w", path, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("assetmapper.VendorLock.Verify: close %s: %w", path, closeErr)
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		if size != pkg.Size || digest != pkg.SHA256 {
			return fmt.Errorf("assetmapper.VendorLock.Verify: %s does not match locked size and SHA-256", specifier)
		}
	}
	return nil
}

func validateLockedPackage(specifier string, pkg LockedPackage) error {
	if pkg.Version == "" {
		return fmt.Errorf("package %q has no exact version", specifier)
	}
	if pkg.Type != "js" && pkg.Type != "css" {
		return fmt.Errorf("package %q has invalid type %q", specifier, pkg.Type)
	}
	if pkg.SourceURL == "" {
		return fmt.Errorf("package %q has no source URL", specifier)
	}
	if pkg.Size < 0 {
		return fmt.Errorf("package %q has invalid size %d", specifier, pkg.Size)
	}
	if pkg.SourceSize < 0 {
		return fmt.Errorf("package %q has invalid source size %d", specifier, pkg.SourceSize)
	}
	if !validSHA256(pkg.SHA256) {
		return fmt.Errorf("package %q has invalid SHA-256", specifier)
	}
	if !validSHA256(pkg.SourceSHA256) {
		return fmt.Errorf("package %q has invalid source SHA-256", specifier)
	}
	if pkg.Path != "" {
		if pkg.Path == "." || !fs.ValidPath(pkg.Path) || strings.ContainsRune(pkg.Path, '\\') {
			return fmt.Errorf("package %q has invalid published path %q", specifier, pkg.Path)
		}
		wantExt := ".js"
		if pkg.Type == "css" {
			wantExt = ".css"
		}
		if filepath.Ext(pkg.Path) != wantExt {
			return fmt.Errorf("package %q published path %q does not match type %q", specifier, pkg.Path, pkg.Type)
		}
	}
	return nil
}

func validSHA256(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}
