package transfer

import (
	"context"
	"io"
	"path"

	"hostbay/internal/sftp"
)

// SFTP is a Source over a host's pooled SFTP client. Overwrites commit with
// posix-rename@openssh.com, which replaces the file in one step.
type SFTP struct{ c sftp.Conn }

// NewSFTP wraps c. Sources over the same SFTP client compare equal, so they
// share one concurrency limit.
func NewSFTP(c *sftp.Conn) SFTP { return SFTP{c: *c} }

func (s SFTP) List(_ context.Context, dir string) ([]Entry, error) { return s.c.List(dir) }
func (s SFTP) Stat(_ context.Context, p string) (Entry, error)     { return s.c.Stat(p) }
func (s SFTP) Open(_ context.Context, p string) (io.ReadCloser, error) {
	return s.c.Open(p)
}

func (s SFTP) Create(_ context.Context, dir, rel string, _ int64) (Writer, error) {
	u, err := s.c.Create(path.Join(dir, rel))
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s SFTP) Mkdir(_ context.Context, dir, rel string) error {
	return s.c.Mkdir(path.Join(dir, rel))
}

func (s SFTP) Rename(_ context.Context, from, to string) error { return s.c.Rename(from, to) }

// Remove deletes as the Files view does: a delete SFTP refuses for
// permission is retried with passwordless sudo.
func (s SFTP) Remove(_ context.Context, p string, isDir bool) error {
	return s.c.Remove(p, isDir, false, "")
}
