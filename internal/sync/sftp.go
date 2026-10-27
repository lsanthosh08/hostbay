package sync

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/pkg/sftp"

	"hostbay/internal/sshx"
)

// NewSFTP syncs through a folder on an SSH server. It dials through
// internal/sshx, so the host key is checked against known_hosts before any
// credential is sent: an unknown key fails with HostKeyUnknown (the app
// offers to trust it only during sync setup) and a changed one with
// HostKeyChanged, which pauses sync (KTD13). secret is the password, or a
// private key when it starts with "-----BEGIN".
func NewSFTP(ctx context.Context, t Target, secret string, d *sshx.Dialer) (Backend, error) {
	if d == nil {
		return nil, errors.New("sftp sync needs a host key checking dialer")
	}
	if t.Host == "" || t.User == "" || t.Path == "" {
		return nil, errors.New("SFTP sync needs a host, a user and a folder")
	}
	auth := sshx.Auth{Password: secret}
	if strings.HasPrefix(strings.TrimSpace(secret), "-----BEGIN") {
		auth = sshx.Auth{Key: []byte(secret)}
	}
	cl, err := d.Dial(ctx, sshx.Target{Key: "sync", Hops: []sshx.Hop{{Host: t.Host, Port: t.Port, User: t.User, Auth: auth}}})
	if err != nil {
		return nil, err
	}
	// The SFTP calls take no context, so ctx (one sync operation's) ends
	// them by closing the link: a stalled server must not hold sync forever.
	stop := context.AfterFunc(ctx, func() { cl.Close() })
	sc, err := sftp.NewClient(cl.Client)
	if err != nil {
		stop()
		cl.Close()
		return nil, err
	}
	return deviceFiles{&sftpFS{c: sc, ssh: cl, dir: t.Path, stop: stop}}, nil
}

type sftpFS struct {
	c    *sftp.Client
	ssh  *sshx.Client
	dir  string
	stop func() bool
}

func (f *sftpFS) List(context.Context) ([]string, error) {
	entries, err := f.c.ReadDir(f.dir)
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

func (f *sftpFS) Read(_ context.Context, name string) ([]byte, error) {
	r, err := f.c.Open(path.Join(f.dir, name))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// Create uploads to a temp name, then renames: SFTP's rename refuses an
// existing target, so nothing is overwritten.
func (f *sftpFS) Create(_ context.Context, name string, data []byte) error {
	if err := f.c.MkdirAll(f.dir); err != nil {
		return err
	}
	tmp := path.Join(f.dir, ".hostbay-"+rand.Text()+".tmp")
	w, err := f.c.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		target := path.Join(f.dir, name)
		if err = f.c.Rename(tmp, target); err != nil {
			if _, serr := f.c.Stat(target); serr == nil {
				err = ErrConflict
			}
		}
	}
	if err != nil {
		f.c.Remove(tmp)
	}
	return err
}

func (f *sftpFS) Remove(_ context.Context, name string) error {
	err := f.c.Remove(path.Join(f.dir, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func (f *sftpFS) Close() error {
	f.stop()
	f.c.Close()
	return f.ssh.Close()
}
