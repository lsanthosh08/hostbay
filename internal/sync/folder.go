package sync

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// NewFolder syncs through a local or shared folder, such as one Dropbox,
// OneDrive or a network drive keeps in step. Each computer writes its own
// files there and never overwrites one (KTD13).
func NewFolder(dir string) Backend { return deviceFiles{folderFS(dir)} }

type folderFS string

func (f folderFS) List(context.Context) ([]string, error) {
	entries, err := os.ReadDir(string(f))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	return names, err
}

func (f folderFS) Read(_ context.Context, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(string(f), name))
}

// Create writes a temp file and hard-links it into place: the link fails
// if the name exists, so nothing is overwritten and no reader sees a
// partial file.
func (f folderFS) Create(_ context.Context, name string, data []byte) error {
	if err := os.MkdirAll(string(f), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(string(f), ".hostbay-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	err = os.Link(tmp.Name(), filepath.Join(string(f), name))
	if errors.Is(err, fs.ErrExist) {
		return ErrConflict
	}
	return err
}

func (f folderFS) Remove(_ context.Context, name string) error {
	err := os.Remove(filepath.Join(string(f), name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (f folderFS) Close() error { return nil }
