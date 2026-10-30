//go:build !windows

package browser

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// locked reads Chromium's SingletonLock, a symlink to "host-pid" that a
// running browser holds. A lock from another host, or one this cannot read,
// counts as live.
func locked(userData string) bool {
	target, err := os.Readlink(filepath.Join(userData, "SingletonLock"))
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	i := strings.LastIndex(target, "-")
	if i < 0 {
		return true
	}
	if host, _ := os.Hostname(); target[:i] != host {
		return true
	}
	pid, err := strconv.Atoi(target[i+1:])
	if err != nil || pid <= 0 {
		return true
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
