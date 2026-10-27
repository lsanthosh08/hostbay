//go:build !windows

package store

import (
	"fmt"
	"os"
	"syscall"
)

// checkFolder refuses a folder another account owns or can write to, so no
// one else can read or swap Hostbay's files.
func checkFolder(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != getuid() {
		return fmt.Errorf("%s belongs to another account", dir)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("other accounts can write to %s; choose a folder only you can change", dir)
	}
	return nil
}

// secureFolder checks dir and gives it the store's mode, 0700 (KTD8).
func secureFolder(dir string) error {
	if err := checkFolder(dir); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}
