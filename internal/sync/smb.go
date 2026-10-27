package sync

import (
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/cloudsoda/go-smb2"

	"hostbay/internal/transfer"
)

// NewSMB syncs through a folder on an SMB share. It dials directly with
// NTLM (transfer.DialSMB); SMB has no host identity check comparable to
// SSH's, so use it on networks you trust.
func NewSMB(ctx context.Context, t Target, password string) (Backend, error) {
	if t.Host == "" || t.Share == "" {
		return nil, errors.New("SMB sync needs a server and a share")
	}
	port := t.Port
	if port == 0 {
		port = 445
	}
	s, err := transfer.DialSMB(ctx, nil, net.JoinHostPort(t.Host, strconv.Itoa(port)), t.User, password, t.Domain)
	if err != nil {
		return nil, err
	}
	share, err := s.Mount(t.Share)
	if err != nil {
		s.Logoff()
		return nil, err
	}
	return deviceFiles{&smbFS{s: s, share: share, dir: strings.Trim(strings.ReplaceAll(t.Path, "/", `\`), `\`)}}, nil
}

type smbFS struct {
	s     *smb2.Session
	share *smb2.Share
	dir   string
}

func (f *smbFS) join(name string) string {
	if f.dir == "" {
		return name
	}
	return f.dir + `\` + name
}

func (f *smbFS) List(ctx context.Context) ([]string, error) {
	dir := f.dir
	if dir == "" {
		dir = "."
	}
	entries, err := f.share.WithContext(ctx).ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var names []string
	for _, e := range entries {
		if e.Mode().IsRegular() {
			names = append(names, e.Name())
		}
	}
	return names, err
}

func (f *smbFS) Read(ctx context.Context, name string) ([]byte, error) {
	return f.share.WithContext(ctx).ReadFile(f.join(name))
}

// Create writes a temp file and renames it into place. SMB's rename would
// replace a file, so the target is checked first; only this computer ever
// writes its own names, and the store lock keeps it to one process.
func (f *smbFS) Create(ctx context.Context, name string, data []byte) error {
	sh := f.share.WithContext(ctx)
	if f.dir != "" {
		if err := sh.MkdirAll(f.dir, 0o700); err != nil {
			return err
		}
	}
	target := f.join(name)
	if _, err := sh.Stat(target); err == nil {
		return ErrConflict
	}
	tmp := f.join(".hostbay-" + rand.Text() + ".tmp")
	w, err := sh.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = sh.Rename(tmp, target)
	}
	if err != nil {
		sh.Remove(tmp)
	}
	return err
}

func (f *smbFS) Remove(ctx context.Context, name string) error {
	err := f.share.WithContext(ctx).Remove(f.join(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (f *smbFS) Close() error {
	f.share.Umount()
	return f.s.Logoff()
}
