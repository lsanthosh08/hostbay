package update

import (
	"crypto/sha256"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// Install checks the verified NSIS installer at path once more through a
// handle that denies write and delete sharing, starts it while that handle
// is still open, and returns. Nothing can swap the file between the check
// and the start. The caller exits Hostbay next so the installer can replace
// it.
func Install(path string, a Asset) error {
	f, err := openVerified(path, a)
	if err != nil {
		return err
	}
	defer f.Close()
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("open")
	// ShellExecute, not CreateProcess: a per-machine installer asks for
	// elevation, which only the shell can request.
	return windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
}

// openVerified opens path for reading with only read sharing, then checks
// its size and SHA-256 against a through that handle.
func openVerified(path string, a Asset) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err == nil {
		err = matches(n, hash.Sum(nil), a)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
