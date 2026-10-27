package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Moving the data folder (KTD42). RequestMove only checks the folder and
// records it in the pointer; Open carries the move out at the next start,
// with the lock held and before any service has the store, so nothing
// writes during the copy. Each step is one atomic pointer write:
//
//	{pending: to} -> copy, verify -> {dir: to, clean: entries} -> next launch: remove entries from the old folder
//
// A crash before the pointer names the new folder leaves the move pending
// and the old folder untouched; the next start copies again. Nothing is
// removed from the old folder until a launch has opened the verified copy.

// moveHook lets tests fail a move, or kill the process, at a named step.
var moveHook func(step string, n int) error

func hook(step string, n int) error {
	if moveHook == nil {
		return nil
	}
	return moveHook(step, n)
}

var getuid = os.Getuid // tests pretend to be another account

// homeOnlyName reports whether name, at the top of DefaultDir, stays there
// whatever the data folder is.
func homeOnlyName(name string) bool {
	return name == lockName || name == pointerName || name == EndpointFile
}

// Dir is the data folder, where every service keeps its files.
func (s *Store) Dir() string { return s.dir }

// Home is DefaultDir: the lock and the pointer stay there.
func (s *Store) Home() string { return s.home }

// Notice explains a move this start could not carry out, or "".
func (s *Store) Notice() string { return s.notice }

// PendingMove is the folder a requested move copies to at the next start,
// or "".
func (s *Store) PendingMove() (string, error) {
	p, err := readPointer(s.home)
	return p.Pending, err
}

// RequestMove checks that to can take Hostbay's data and records it as a
// pending move, which the next start carries out. It refuses a folder that
// is not empty, is the current folder or inside it, belongs to another
// account or that other accounts can write to; on Windows it then gives the
// folder a protected DACL for the user, SYSTEM and Administrators only.
func (s *Store) RequestMove(to string) error {
	if !filepath.IsAbs(to) {
		return fmt.Errorf("%s is not a full path", to)
	}
	to = filepath.Clean(to)
	fi, err := os.Stat(to)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: no such folder", to)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder", to)
	}
	if samePath(to, s.dir) {
		return fmt.Errorf("Hostbay's data is already in %s", to)
	}
	if inside(to, s.dir) {
		return fmt.Errorf("%s is inside the current data folder %s", to, s.dir)
	}
	p, err := readPointer(s.home)
	if err != nil {
		return err
	}
	ignore := func(string) bool { return false }
	if samePath(to, s.home) {
		// Moving back: what an earlier move left here for cleanup is removed
		// at the next start, before this move copies.
		var leftover []string
		if p.CleanReady && samePath(p.CleanFrom, to) {
			leftover = p.Clean
		}
		ignore = func(name string) bool { return homeOnlyName(name) || slices.Contains(leftover, name) }
	}
	entries, err := os.ReadDir(to)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !ignore(e.Name()) {
			return fmt.Errorf("%s is not empty; choose an empty folder", to)
		}
	}
	if err := secureFolder(to); err != nil {
		return err
	}
	p.Pending = to
	return writePointer(s.home, p)
}

// carryOut copies the data folder to p.Pending. On success the store's
// folder is the copy and the returned pointer names it; on failure the old
// folder stays in use, s.notice says why and the old pointer is returned.
func (s *Store) carryOut(p pointer) pointer {
	from, to := s.dir, p.Pending
	plan, err := listEntries(from)
	if err == nil {
		err = checkTarget(to, plan, s.home)
		if err == nil {
			err = copyEntries(from, to, plan)
			if err == nil {
				err = verifyEntries(from, to, plan)
			}
			if err == nil {
				err = hook("verified", 0)
			}
			if err != nil {
				// checkTarget showed every entry named in plan is this move's own.
				err = errors.Join(err, removeEntries(to, plan, ""))
			}
		}
	}
	if err != nil {
		s.notice = fmt.Sprintf("Hostbay could not move its data to %s and is still using %s: %v", to, from, err)
		p.Pending = ""
		if werr := writePointer(s.home, p); werr != nil {
			s.notice += fmt.Sprintf(" (the move will be tried again at the next start: %v)", werr)
		}
		return p
	}
	next := pointer{CleanFrom: from, Clean: plan}
	if !samePath(to, s.home) {
		next.Dir = to
	}
	if err := writePointer(s.home, next); err != nil {
		// A failed flush can follow a rename that landed; only a pointer that
		// still names the old folder keeps it in use.
		if cur, rerr := readPointer(s.home); rerr != nil || cur.Pending != "" || cur.Dir != next.Dir {
			s.notice = fmt.Sprintf("Hostbay copied its data to %s but could not record the move, so it is still using %s and deleted nothing: %v", to, from, err)
			return p
		}
	}
	s.dir = next.dir(s.home)
	return next
}

// cleanUp removes from p.CleanFrom the entries a move copied, now that a
// launch has opened the copy. It never touches the folder in use, the
// folder itself or the files that stay in DefaultDir.
func (s *Store) cleanUp(p pointer) pointer {
	if !samePath(p.CleanFrom, s.dir) {
		if _, err := os.Stat(p.CleanFrom); err != nil {
			return p // unplugged or gone: try again next launch
		}
		if err := removeEntries(p.CleanFrom, p.Clean, "clean"); err != nil {
			s.notice = fmt.Sprintf("Hostbay could not remove all of its old copy in %s: %v", p.CleanFrom, err)
		}
	}
	p.CleanFrom, p.Clean, p.CleanReady = "", nil, false
	if err := writePointer(s.home, p); err != nil {
		s.notice = fmt.Sprintf("Hostbay could not record the cleanup of its old folder: %v", err)
	}
	return p
}

// listEntries lists every file and folder under dir, parents before their
// contents, leaving out the files that stay in DefaultDir. WalkDir never
// follows a link, and copyEntries refuses one rather than drop it.
func listEntries(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		if !homeOnlyName(rel) {
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

// checkTarget makes sure to holds nothing but entries from plan, left by an
// interrupted copy, and, when it is DefaultDir, the files kept there.
func checkTarget(to string, plan []string, home string) error {
	entries, err := os.ReadDir(to)
	if err != nil {
		return err
	}
	isHome := samePath(to, home)
	for _, e := range entries {
		if n := e.Name(); !slices.Contains(plan, n) && !(isHome && homeOnlyName(n)) {
			return fmt.Errorf("%s is no longer empty", to)
		}
	}
	return nil
}

// copyEntries copies plan from one folder to the other through os.Root,
// so no path can lead outside either, and flushes every file and folder.
func copyEntries(from, to string, plan []string) error {
	src, err := os.OpenRoot(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenRoot(to)
	if err != nil {
		return err
	}
	defer dst.Close()
	var dirs []string
	for i, rel := range plan {
		fi, err := src.Lstat(rel)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			if err := dst.Mkdir(rel, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			dirs = append(dirs, rel)
		case fi.Mode().IsRegular():
			if err := copyFile(src, dst, rel); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is a link or special file; Hostbay moves only files and folders", rel)
		}
		if err := hook("copy", i+1); err != nil {
			return err
		}
	}
	for _, d := range append(dirs, ".") {
		if err := syncRootDir(dst, d); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst *os.Root, rel string) error {
	in, err := src.Open(rel)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := dst.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func syncRootDir(r *os.Root, rel string) error {
	if runtime.GOOS == "windows" {
		return nil // as syncDir
	}
	d, err := r.Open(rel)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// verifyEntries reads every copied file back from disk and compares it
// with the original.
func verifyEntries(from, to string, plan []string) error {
	src, err := os.OpenRoot(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenRoot(to)
	if err != nil {
		return err
	}
	defer dst.Close()
	for _, rel := range plan {
		a, err := src.Lstat(rel)
		if err != nil {
			return err
		}
		b, err := dst.Lstat(rel)
		if err != nil {
			return fmt.Errorf("copy of %s: %w", rel, err)
		}
		if a.IsDir() != b.IsDir() {
			return fmt.Errorf("copy of %s does not match", rel)
		}
		if a.IsDir() {
			continue
		}
		ha, err := hashFile(src, rel)
		if err != nil {
			return err
		}
		hb, err := hashFile(dst, rel)
		if err != nil {
			return err
		}
		if !bytes.Equal(ha, hb) {
			return fmt.Errorf("copy of %s does not match", rel)
		}
	}
	return nil
}

func hashFile(r *os.Root, rel string) ([]byte, error) {
	f, err := r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// removeEntries removes the entries of plan from dir, contents before their
// folders, through os.Root. A folder that still holds something not in plan
// is left in place. step names the test hook called after each removal.
func removeEntries(dir string, plan []string, step string) error {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer r.Close()
	var errs []error
	n := 0
	for _, rel := range slices.Backward(plan) {
		if !filepath.IsLocal(rel) || homeOnlyName(rel) {
			continue // a pointer that was tampered with
		}
		err := r.Remove(rel)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n++
		if step != "" {
			if err := hook(step, n); err != nil {
				return err
			}
		}
	}
	return errors.Join(errs...)
}

// resolve gives the form two paths are compared in: links resolved, and
// case folded where the file system usually ignores case.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	p = filepath.Clean(p)
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

func samePath(a, b string) bool { return resolve(a) == resolve(b) }

// inside reports whether child is somewhere below parent.
func inside(child, parent string) bool {
	rel, err := filepath.Rel(resolve(parent), resolve(child))
	return err == nil && rel != "." && filepath.IsLocal(rel)
}
