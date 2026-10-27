//go:build !windows

package claudecode

import (
	"io/fs"
	"os"
	"syscall"
)

func fileOwner(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

func currentUID() uint32 { return uint32(os.Getuid()) }
