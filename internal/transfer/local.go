package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// Local is this computer's disk. Writes go through an os.Root for the
// folder being written into, after a filepath.IsLocal check, so a name a
// server sent cannot land outside it (the CVE-2019-6111 class).
//
// shortcut: paths are slash-separated and joined with package path, which
// collapses a Windows UNC path's leading //; handle UNC roots when Windows
// file panes need network shares.
type Local struct{}

func (Local) List(_ context.Context, dir string) ([]Entry, error) {
	des, err := os.ReadDir(filepath.FromSlash(dir))
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		fi, err := de.Info()
		if err != nil {
			continue // removed since the read
		}
		out = append(out, localEntry(de.Name(), path.Join(dir, de.Name()), fi))
	}
	sortEntries(out)
	return out, nil
}

func (Local) Stat(_ context.Context, p string) (Entry, error) {
	fi, err := os.Stat(filepath.FromSlash(p))
	if err != nil {
		return Entry{}, err
	}
	return localEntry(path.Base(p), p, fi), nil
}

func (Local) Open(_ context.Context, p string) (io.ReadCloser, error) {
	return os.Open(filepath.FromSlash(p))
}

func (Local) Create(_ context.Context, dir, rel string, _ int64) (Writer, error) {
	root, name, err := openRoot(dir, rel)
	if err != nil {
		return nil, err
	}
	dst, err := resolveIn(root, name)
	if err != nil {
		root.Close()
		return nil, err
	}
	tmp := filepath.Join(filepath.Dir(dst), tempName())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		root.Close()
		return nil, err
	}
	w := &localWriter{root: root, f: f, tmp: tmp, dst: dst}
	if fi, err := root.Stat(dst); err == nil {
		if err := f.Chmod(fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)); err != nil {
			w.Abort()
			return nil, err
		}
	}
	return w, nil
}

func (Local) Mkdir(_ context.Context, dir, rel string) error {
	root, name, err := openRoot(dir, rel)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Mkdir(name, 0o755)
}

func (Local) Rename(_ context.Context, from, to string) error {
	return os.Rename(filepath.FromSlash(from), filepath.FromSlash(to))
}

func (Local) Remove(_ context.Context, p string, _ bool) error {
	return os.Remove(filepath.FromSlash(p))
}

// openRoot checks that rel stays inside dir and opens dir as an os.Root.
// It returns rel in the OS's form.
func openRoot(dir, rel string) (*os.Root, string, error) {
	name := filepath.FromSlash(rel)
	if !filepath.IsLocal(name) {
		return nil, "", fmt.Errorf("%q is outside the destination folder", rel)
	}
	root, err := os.OpenRoot(filepath.FromSlash(dir))
	return root, name, err
}

// resolveIn follows name through symlinks to the file they name, refusing
// one that leads out of root.
func resolveIn(root *os.Root, name string) (string, error) {
	for range 40 {
		fi, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) || err == nil && fi.Mode()&fs.ModeSymlink == 0 {
			return name, nil
		}
		if err != nil {
			return "", err
		}
		t, err := root.Readlink(name)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(t) {
			if t, err = filepath.Rel(root.Name(), t); err != nil {
				return "", err
			}
		} else {
			t = filepath.Join(filepath.Dir(name), t)
		}
		if !filepath.IsLocal(t) {
			return "", fmt.Errorf("%s links outside the destination folder", name)
		}
		name = t
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", name)
}

// localWriter writes a temp file beside dst and renames it into place.
type localWriter struct {
	root     *os.Root
	f        *os.File
	tmp, dst string
}

func (w *localWriter) Write(b []byte) (int, error) { return w.f.Write(b) }

func (w *localWriter) Commit() error {
	defer w.root.Close()
	err := w.f.Sync()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = w.root.Rename(w.tmp, w.dst)
	}
	if err != nil {
		w.root.Remove(w.tmp)
	}
	return err
}

func (w *localWriter) Abort() error {
	defer w.root.Close()
	w.f.Close()
	return w.root.Remove(w.tmp)
}

// localEntry describes fi with a raw st_mode, as SFTP reports it.
func localEntry(name, p string, fi fs.FileInfo) Entry {
	m := fi.Mode()
	mode := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		mode |= 0o1000
	}
	switch {
	case m.IsDir():
		mode |= modeDir
	case m&fs.ModeSymlink != 0:
		mode |= modeSymlink
	case m.IsRegular():
		mode |= 0o100000
	}
	return Entry{Name: name, Path: p, IsDir: m.IsDir(), Size: uint64(fi.Size()), Mode: mode, Mtime: fi.ModTime().Unix()}
}
