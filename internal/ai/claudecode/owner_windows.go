//go:build windows

package claudecode

import "io/fs"

// Windows has no owner check: the path rules there are the .exe rule and
// the sign-in being the person's own.
func fileOwner(fs.FileInfo) (uint32, bool) { return 0, false }

func currentUID() uint32 { return 0 }
