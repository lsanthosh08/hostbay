//go:build !windows

package mcp

import (
	"os"
	"syscall"
)

// alive reports whether process pid exists.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}
