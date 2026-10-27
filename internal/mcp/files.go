package mcp

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// CheckFolder refuses an MCP folder that equals, contains or sits inside
// the data folder or ~/.ssh (KTD36), compared after resolving symlinks,
// and case-insensitively on macOS and Windows.
func CheckFolder(dir, dataDir, sshDir string) error {
	d, err := resolve(dir)
	if err != nil {
		return fmt.Errorf("the MCP folder cannot be used: %w", err)
	}
	for _, refused := range []struct{ path, what string }{{dataDir, "Hostbay's data folder"}, {sshDir, "~/.ssh"}} {
		r, err := resolve(refused.path)
		if err != nil {
			r = filepath.Clean(refused.path) // a missing ~/.ssh still may not be picked
		}
		if overlaps(d, r) {
			return fmt.Errorf("the MCP folder cannot be %s, inside it or a folder that holds it", refused.what)
		}
	}
	return nil
}

func resolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// overlaps reports whether a and b are the same folder or one holds the other.
func overlaps(a, b string) bool {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	inside := func(x, y string) bool {
		rel, err := filepath.Rel(y, x)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	}
	return inside(a, b) || inside(b, a)
}

// localName checks a path the client gives inside the MCP folder: relative,
// with no way out. os.Root enforces it again on every open, symlinks too.
func localName(p string) (string, error) {
	if p == "" || filepath.IsAbs(p) || filepath.VolumeName(p) != "" || !filepath.IsLocal(filepath.FromSlash(p)) {
		return "", fmt.Errorf("%q is not a path inside the MCP folder; give one relative to it", p)
	}
	return filepath.FromSlash(p), nil
}

// openLocal opens name in folder for an upload.
func openLocal(folder, name string) (io.ReadCloser, error) {
	root, err := os.OpenRoot(folder)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a file", name)
	}
	return f, nil
}

// createLocal makes name in folder for a download; it never replaces a
// file that exists.
func createLocal(folder, name string) (io.WriteCloser, func(), error) {
	root, err := os.OpenRoot(folder)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil, nil, fmt.Errorf("%s already exists in the MCP folder; downloads never replace a file", name)
	}
	if err != nil {
		return nil, nil, err
	}
	// Through a root, as every open: the client may have swapped a folder on
	// the way for a link elsewhere since the file was made.
	return f, func() {
		if r, err := os.OpenRoot(folder); err == nil {
			_ = r.Remove(name)
			r.Close()
		}
	}, nil
}
