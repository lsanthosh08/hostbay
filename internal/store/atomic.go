package store

import (
	"os"
	"path/filepath"
	"runtime"
)

// writeFileAtomic replaces dir/name with data, so a reader sees either the
// old file or the new one, never a partial write. The file is mode 0600.
func writeFileAtomic(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // fails harmlessly once renamed
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil // Windows cannot fsync a directory; NTFS journals the rename
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
