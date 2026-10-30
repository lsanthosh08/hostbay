package browser

import (
	"errors"
	"os"
	"path/filepath"
)

// locked tries Chromium's "lockfile", which a running browser keeps open
// without sharing: opening it fails while the browser runs.
// shortcut: not checked against a real Windows Chrome yet; verify on Windows before relying on it there.
func locked(userData string) bool {
	f, err := os.OpenFile(filepath.Join(userData, "lockfile"), os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	f.Close()
	return false
}
