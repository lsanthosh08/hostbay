package update

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Install replaces the running AppImage ($APPIMAGE) with the verified one at
// path. deb and rpm installs need root, so they never download: the UI links
// the release page instead. The new version runs from the next launch.
func Install(path string, _ Asset) error {
	target := os.Getenv("APPIMAGE")
	if target == "" {
		return errors.New("this install updates from the release page")
	}
	return replaceFile(path, target)
}

// replaceFile copies src beside dst, then renames it over dst, so dst is
// either the old file or the whole new one.
func replaceFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".hostbay-update-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	_, err = io.Copy(tmp, in)
	if err == nil {
		err = tmp.Chmod(0o755)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
