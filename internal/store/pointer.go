package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// These stay in DefaultDir whatever the data folder is (KTD42). None ends
// in .json, so storeFiles never takes them for store documents.
const (
	lockName    = "hostbay.lock"
	pointerName = "datafolder"
	// EndpointFile is where the MCP server writes its port, beside the lock.
	EndpointFile = "mcp-endpoint"
)

const pointerVersion = 1

// pointer is the one file that says where the data folder is and what a
// move still has to do. Every step of a move is a single atomic write of
// it, so a crash leaves either the step before or the step after.
type pointer struct {
	Version int    `json:"version"`
	Dir     string `json:"dir,omitempty"`     // the data folder; empty means DefaultDir
	Pending string `json:"pending,omitempty"` // a requested move, carried out at the next start
	// The entries a move copied out of CleanFrom, removed from there once a
	// later launch has opened the copy (CleanReady).
	CleanFrom  string   `json:"cleanFrom,omitempty"`
	Clean      []string `json:"clean,omitempty"`
	CleanReady bool     `json:"cleanReady,omitempty"`
}

func (p pointer) dir(home string) string {
	if p.Dir == "" {
		return home
	}
	return p.Dir
}

// readPointer reads home's pointer; a store never moved has none.
func readPointer(home string) (pointer, error) {
	var p pointer
	b, err := os.ReadFile(filepath.Join(home, pointerName))
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("%s: %w", pointerName, err)
	}
	if p.Version > pointerVersion {
		return p, fmt.Errorf("%s: %w", pointerName, ErrNewerSchema)
	}
	return p, nil
}

// writePointer saves p, or removes the pointer when p says nothing beyond
// "the data is in DefaultDir".
func writePointer(home string, p pointer) error {
	if err := hook("pointer", 0); err != nil {
		return err
	}
	if p.Dir == "" && p.Pending == "" && len(p.Clean) == 0 {
		err := os.Remove(filepath.Join(home, pointerName))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return syncDir(home)
	}
	p.Version = pointerVersion
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(home, pointerName, b)
}

// FolderMissingError means the pointer names a data folder that is gone or
// unreadable. Hostbay stops rather than open an empty store in its place.
type FolderMissingError struct {
	Dir string
	Err error
}

func (e *FolderMissingError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("Hostbay's data folder %s cannot be read: %v", e.Dir, e.Err)
	}
	return fmt.Sprintf("Hostbay's data folder %s is missing or holds no Hostbay data", e.Dir)
}

func (e *FolderMissingError) Unwrap() error { return e.Err }

// HoldsStore reports whether dir has a Hostbay store: Open writes
// state.json into every store it opens.
func HoldsStore(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, stateName))
	return err == nil && fi.Mode().IsRegular()
}

// PointTo makes dir, which must already hold a Hostbay store, the data
// folder for home, dropping any pending move or cleanup. It is the
// missing-folder dialog's "Choose another folder".
func PointTo(home, dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%s is not a full path", dir)
	}
	if !HoldsStore(dir) {
		return fmt.Errorf("%s holds no Hostbay data", dir)
	}
	if err := checkFolder(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	lock, err := acquireLock(home)
	if err != nil {
		return err
	}
	defer lock.Close()
	var p pointer
	if !samePath(dir, home) {
		p.Dir = filepath.Clean(dir)
	}
	return writePointer(home, p)
}
