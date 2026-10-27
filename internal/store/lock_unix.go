//go:build !windows

package store

import (
	"errors"
	"os"
	"syscall"
)

// lockFile uses flock, which belongs to the open file, so a second open of
// the lock file is refused even inside the same process.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}
