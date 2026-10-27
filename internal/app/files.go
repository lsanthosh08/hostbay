package app

import (
	"context"
	"errors"
	"io"
	"path"
	"path/filepath"

	"hostbay/internal/sftp"
	"hostbay/internal/sshx"
	"hostbay/internal/transfer"
)

// FilesService browses and edits remote files over SFTP (R4), or SCP when
// the server has no SFTP (R38), and saves root-owned files through sudo
// (R5). sessionID is the lease ID that ConnService.Connect returned.
type FilesService struct {
	mgr  *sshx.Manager
	pool sftp.Pool
}

func NewFilesService(mgr *sshx.Manager) *FilesService { return &FilesService{mgr: mgr} }

// conn returns the SFTP connection for a session and the password sudo
// falls back to. A dropped connection is rebuilt first; the pool then opens
// a fresh SFTP client on it, since the old one died with the old connection.
// When the server has no SFTP (R38), scp is set, and c can only run the
// shell commands behind Copy, Chmod and sudo Remove.
func (s *FilesService) conn(sessionID string) (c *sftp.Conn, scp transfer.Source, pw string, err error) {
	l := s.mgr.Lease(sessionID)
	if l == nil {
		return nil, nil, "", errors.New("session not found")
	}
	cl, err := l.Live(context.Background())
	if err != nil {
		return nil, nil, "", err
	}
	c, err = s.pool.Get(cl.Client)
	if errors.Is(err, sftp.ErrNoSFTP) {
		return sftp.Shell(cl.Client), transfer.NewSCP(cl.Client), l.LoginPassword(), nil
	}
	return c, nil, l.LoginPassword(), err
}

// source returns the session's files as a transfer source: SFTP, or SCP
// when the server has no SFTP.
func (s *FilesService) source(sessionID string) (transfer.Source, string, error) {
	c, scp, _, err := s.conn(sessionID)
	switch {
	case err != nil:
		return nil, "", err
	case scp != nil:
		return scp, "scp", nil
	}
	return transfer.NewSFTP(c), "sftp", nil
}

// split cuts a remote path into its folder and name.
func split(p string) (dir, name string) { return path.Dir(p), path.Base(p) }

func (s *FilesService) SftpList(sessionID, path string) ([]DirEntry, error) {
	c, scp, _, err := s.conn(sessionID)
	if err != nil {
		return nil, err
	}
	var entries []sftp.Entry
	if scp != nil {
		entries, err = scp.List(context.Background(), path)
	} else {
		entries, err = c.List(path)
	}
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, len(entries))
	for i, e := range entries {
		out[i] = DirEntry(e)
	}
	return out, nil
}

func (s *FilesService) SftpRead(sessionID, path string) (string, error) {
	c, scp, _, err := s.conn(sessionID)
	if err != nil {
		return "", err
	}
	if scp != nil {
		r, err := scp.Open(context.Background(), path)
		if err != nil {
			return "", err
		}
		defer r.Close()
		b, err := io.ReadAll(r)
		return string(b), err
	}
	return c.Read(path)
}

// SftpWrite saves content. sudo uses sudoPassword when given, else the
// host's saved login password, else passwordless sudo. Without SFTP it
// saves over SCP, which cannot stage a sudo save.
func (s *FilesService) SftpWrite(sessionID, path, content string, sudo bool, sudoPassword *string) error {
	c, scp, pw, err := s.conn(sessionID)
	if err != nil {
		return err
	}
	if scp != nil {
		if sudo {
			return errors.New("saving with sudo needs SFTP, which this server does not offer")
		}
		ctx := context.Background()
		dir, name := split(path)
		w, err := scp.Create(ctx, dir, name, int64(len(content)))
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, content); err != nil {
			w.Abort()
			return err
		}
		return w.Commit()
	}
	if sudoPassword != nil && *sudoPassword != "" {
		pw = *sudoPassword
	}
	return c.Write(path, content, sudo, pw)
}

func (s *FilesService) SftpMkdir(sessionID, path string) error {
	c, scp, _, err := s.conn(sessionID)
	if err != nil {
		return err
	}
	if scp != nil {
		dir, name := split(path)
		return scp.Mkdir(context.Background(), dir, name)
	}
	return c.Mkdir(path)
}

func (s *FilesService) SftpRename(sessionID, from, to string) error {
	c, scp, _, err := s.conn(sessionID)
	if err != nil {
		return err
	}
	if scp != nil {
		return scp.Rename(context.Background(), from, to)
	}
	return c.Rename(from, to)
}

func (s *FilesService) SftpRemove(sessionID, path string, isDir, sudo bool) error {
	c, scp, pw, err := s.conn(sessionID)
	if err != nil {
		return err
	}
	if scp != nil && !sudo {
		return scp.Remove(context.Background(), path, isDir)
	}
	return c.Remove(path, isDir, sudo, pw)
}

func (s *FilesService) SftpCopy(sessionID, from, to string, sudo bool) error {
	c, _, pw, err := s.conn(sessionID)
	if err != nil {
		return err
	}
	return c.Copy(from, to, sudo, pw)
}

func (s *FilesService) SftpChmod(sessionID, path string, mode uint32, sudo bool) error {
	c, _, pw, err := s.conn(sessionID)
	if err != nil {
		return err
	}
	return c.Chmod(path, mode, sudo, pw)
}

func (s *FilesService) SftpDownload(sessionID, remote, local string) error {
	c, scp, _, err := s.conn(sessionID)
	if err != nil {
		return err
	}
	if scp != nil {
		dir, name := filepath.Split(local)
		return transfer.CopyFile(context.Background(), scp, remote, transfer.Local{}, filepath.ToSlash(dir), name)
	}
	return c.Download(remote, local)
}

func (s *FilesService) SftpUpload(sessionID, local, remote string) error {
	c, scp, _, err := s.conn(sessionID)
	if err != nil {
		return err
	}
	if scp != nil {
		dir, name := split(remote)
		return transfer.CopyFile(context.Background(), transfer.Local{}, filepath.ToSlash(local), scp, dir, name)
	}
	return c.Upload(local, remote)
}
