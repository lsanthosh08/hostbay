package store

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrLocked means another Hostbay process holds the store.
var ErrLocked = errors.New("another Hostbay is already running; switch to its window")

// acquireLock takes an OS-level exclusive lock on dir/hostbay.lock, where dir
// is DefaultDir whatever the data folder is (KTD42). The OS
// drops it when the file is closed or the process exits, even after a crash.
func acquireLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
