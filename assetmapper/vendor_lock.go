package assetmapper

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// VendorLockFilename is the provenance record written beside VendorDir.
const VendorLockFilename = "vendor.lock.json"

const vendorLockVersion = 1

// VendorLock records the exact source and published bytes of vendored files.
type VendorLock struct {
	Version  int                      `json:"version"`
	Packages map[string]LockedPackage `json:"packages"`
}

// LockedPackage records one published vendored artifact.
type LockedPackage struct {
	Version      string `json:"version"`
	Type         string `json:"type"`
	SourceURL    string `json:"source_url"`
	SourceSize   int64  `json:"source_size"`
	SourceSHA256 string `json:"source_sha256"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

func newVendorLock() *VendorLock {
	return &VendorLock{
		Version:  vendorLockVersion,
		Packages: map[string]LockedPackage{},
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
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return nil, fmt.Errorf("assetmapper.LoadVendorLock: decode %s: %w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("assetmapper.LoadVendorLock: decode %s: multiple JSON values", path)
		}
		return nil, fmt.Errorf("assetmapper.LoadVendorLock: decode %s: %w", path, err)
	}
	if lock.Version != vendorLockVersion {
		return nil, fmt.Errorf("assetmapper.LoadVendorLock: unsupported version %d", lock.Version)
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
	file, err := os.Create(path) // #nosec G304 -- writes a caller-selected project lockfile
	if err != nil {
		return fmt.Errorf("assetmapper.VendorLock.Save: create %s: %w", path, err)
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("assetmapper.VendorLock.Save: %w", err)
	}
	data = append(data, '\n')
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("assetmapper.VendorLock.Save: write %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("assetmapper.VendorLock.Save: close %s: %w", path, err)
	}
	return nil
}

// Verify checks every locked artifact against its recorded size and digest.
func (l *VendorLock) Verify(vendorDir string) error {
	for specifier, pkg := range l.Packages {
		rel, err := vendorRelPath(specifier, pkg.Type)
		if err != nil {
			return err
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
	return nil
}

func validSHA256(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}
