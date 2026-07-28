package assetmapper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// VendorTransactionFilename records a metadata commit that must be completed
// after an interrupted vendoring operation.
const VendorTransactionFilename = "vendor.transaction.json"

type vendorTransaction struct {
	Version int                       `json:"version"`
	Lock    *VendorLock               `json:"lock"`
	Entries map[string]ImportmapEntry `json:"entries"`
}

func (v *Vendor) publishMetadata(lock *VendorLock, next *Importmap) error {
	transaction := vendorTransaction{Version: 1, Lock: lock, Entries: next.Entries}
	data, err := json.MarshalIndent(transaction, "", "  ")
	if err != nil {
		return fmt.Errorf("assetmapper.Vendor: encode transaction: %w", err)
	}
	data = append(data, '\n')
	journal := v.transactionPath()
	if err := atomicWriteFile(journal, data, 0o644); err != nil {
		return fmt.Errorf("assetmapper.Vendor: write transaction journal: %w", err)
	}
	if err := lock.Save(v.lockPath()); err != nil {
		return err
	}
	if err := next.Save(v.importmapPath()); err != nil {
		return err
	}
	v.Importmap.Entries = next.Entries
	return removeTransactionJournal(journal)
}

func (v *Vendor) recoverTransaction() error {
	journal := v.transactionPath()
	file, err := os.Open(journal) // #nosec G304 -- derived project metadata path
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("assetmapper.Vendor: open transaction journal: %w", err)
	}
	var transaction vendorTransaction
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&transaction); err != nil {
		_ = file.Close()
		return fmt.Errorf("assetmapper.Vendor: decode transaction journal: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		_ = file.Close()
		if err == nil {
			return fmt.Errorf("assetmapper.Vendor: transaction journal contains multiple JSON values")
		}
		return fmt.Errorf("assetmapper.Vendor: decode transaction journal: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("assetmapper.Vendor: close transaction journal: %w", err)
	}
	if transaction.Version != 1 || transaction.Lock == nil || transaction.Entries == nil {
		return fmt.Errorf("assetmapper.Vendor: invalid transaction journal")
	}
	if transaction.Lock.Version != vendorLockVersion {
		return fmt.Errorf("assetmapper.Vendor: transaction journal has unsupported lock version %d", transaction.Lock.Version)
	}
	for specifier, pkg := range transaction.Lock.Packages {
		if _, err := vendorRelPath(specifier, pkg.Type); err != nil {
			return fmt.Errorf("assetmapper.Vendor: invalid transaction journal: %w", err)
		}
		if err := validateLockedPackage(specifier, pkg); err != nil {
			return fmt.Errorf("assetmapper.Vendor: invalid transaction journal: %w", err)
		}
	}
	if err := transaction.Lock.Verify(v.VendorDir); err != nil {
		return fmt.Errorf("assetmapper.Vendor: recover transaction: %w", err)
	}
	next := &Importmap{Entries: transaction.Entries}
	if err := transaction.Lock.Save(v.lockPath()); err != nil {
		return err
	}
	if err := next.Save(v.importmapPath()); err != nil {
		return err
	}
	v.Importmap.Entries = transaction.Entries
	return removeTransactionJournal(journal)
}

func (v *Vendor) transactionPath() string {
	return filepath.Join(filepath.Dir(v.importmapPath()), VendorTransactionFilename)
}

func removeTransactionJournal(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return &IndeterminateCommitError{Path: path, Err: fmt.Errorf("remove completed transaction journal: %w", err)}
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return &IndeterminateCommitError{Path: path, Err: fmt.Errorf("sync transaction journal removal: %w", err)}
	}
	return nil
}
