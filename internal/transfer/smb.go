package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/cloudsoda/go-smb2"
)

// DialSMB signs in to an SMB server with NTLM. SMB has no host identity
// check comparable to SSH's, so use it on networks you trust. Sync's SMB
// target dials through it too, with a nil dial (direct).
func DialSMB(ctx context.Context, dial DialFunc, addr, user, password, domain string) (*smb2.Session, error) {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, err := dial(dctx, "tcp", addr)
	cancel()
	if err != nil {
		return nil, err
	}
	s, err := (&smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: user, Password: password, Domain: domain}}).DialConn(ctx, conn, addr)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

// SMB is a Source on an SMB server (R40). "/" lists its shares and
// "/share/folder" is a folder in one. Writes go to a temporary name and
// commit with SMB's rename, which replaces the file in one step.
type SMB struct {
	s *smb2.Session

	mu     sync.Mutex
	shares map[string]*smb2.Share
}

// NewSMB wraps a signed-in session; Close logs it off.
func NewSMB(s *smb2.Session) *SMB { return &SMB{s: s, shares: map[string]*smb2.Share{}} }

func (m *SMB) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, sh := range m.shares {
		sh.Umount()
	}
	m.shares = nil
	return m.s.Logoff()
}

// share mounts the share p is in, once, and returns p inside it with
// SMB's separators.
func (m *SMB) share(ctx context.Context, p string) (*smb2.Share, string, error) {
	name, rest, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	if name == "" {
		return nil, "", errors.New("pick a share first")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shares == nil {
		return nil, "", errors.New("the SMB connection is closed")
	}
	sh := m.shares[name]
	if sh == nil {
		var err error
		if sh, err = m.s.Mount(name); err != nil {
			return nil, "", err
		}
		m.shares[name] = sh
	}
	return sh.WithContext(ctx), strings.ReplaceAll(strings.Trim(rest, "/"), "/", `\`), nil
}

func (m *SMB) List(ctx context.Context, dir string) ([]Entry, error) {
	if strings.Trim(dir, "/") == "" {
		names, err := m.s.WithContext(ctx).ListSharenames()
		if err != nil {
			return nil, err
		}
		out := []Entry{}
		for _, n := range names {
			if n != "IPC$" {
				out = append(out, Entry{Name: n, Path: "/" + n, IsDir: true, Mode: modeDir | 0o755})
			}
		}
		sortEntries(out)
		return out, nil
	}
	sh, p, err := m.share(ctx, dir)
	if err != nil {
		return nil, err
	}
	fis, err := sh.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(fis))
	for _, fi := range fis {
		out = append(out, localEntry(fi.Name(), joinRaw(dir, fi.Name()), fi))
	}
	sortEntries(out)
	return out, nil
}

func (m *SMB) Stat(ctx context.Context, p string) (Entry, error) {
	name := strings.Trim(p, "/")
	if name == "" || !strings.Contains(name, "/") {
		// The server root or a share: a share exists if it mounts.
		if name != "" {
			if _, _, err := m.share(ctx, p); err != nil {
				return Entry{}, fmt.Errorf("%w: %w", fs.ErrNotExist, err)
			}
		}
		return Entry{Name: path.Base(p), Path: p, IsDir: true, Mode: modeDir | 0o755}, nil
	}
	sh, sp, err := m.share(ctx, p)
	if err != nil {
		return Entry{}, err
	}
	fi, err := sh.Stat(sp)
	if err != nil {
		return Entry{}, err
	}
	return localEntry(path.Base(p), p, fi), nil
}

func (m *SMB) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	sh, sp, err := m.share(ctx, p)
	if err != nil {
		return nil, err
	}
	return sh.Open(sp)
}

func (m *SMB) Create(ctx context.Context, dir, rel string, _ int64) (Writer, error) {
	dst := path.Join(dir, rel)
	sh, sdst, err := m.share(ctx, dst)
	if err != nil {
		return nil, err
	}
	_, stmp, _ := m.share(ctx, path.Join(path.Dir(dst), tempName()))
	f, err := sh.OpenFile(stmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	return &smbWriter{sh: sh, f: f, tmp: stmp, dst: sdst}, nil
}

type smbWriter struct {
	sh       *smb2.Share
	f        *smb2.File
	tmp, dst string
}

func (w *smbWriter) Write(b []byte) (int, error) { return w.f.Write(b) }

func (w *smbWriter) Commit() error {
	err := w.f.Close()
	if err == nil {
		err = w.sh.Rename(w.tmp, w.dst)
	}
	if err != nil {
		w.sh.Remove(w.tmp)
	}
	return err
}

func (w *smbWriter) Abort() error {
	w.f.Close()
	// A cancel has already ended the task's context.
	return w.sh.WithContext(context.Background()).Remove(w.tmp)
}

func (m *SMB) Mkdir(ctx context.Context, dir, rel string) error {
	sh, p, err := m.share(ctx, path.Join(dir, rel))
	if err != nil {
		return err
	}
	return sh.Mkdir(p, 0o755)
}

// Rename refuses to replace an existing file, as the other sources do;
// SMB's own rename would.
func (m *SMB) Rename(ctx context.Context, from, to string) error {
	shareOf := func(p string) string { n, _, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/"); return n }
	if shareOf(from) != shareOf(to) {
		return errors.New("SMB cannot move a file to another share")
	}
	sh, sfrom, err := m.share(ctx, from)
	if err != nil {
		return err
	}
	_, sto, err := m.share(ctx, to)
	if err != nil {
		return err
	}
	if _, err := sh.Lstat(sto); err == nil {
		return &os.LinkError{Op: "rename", Old: from, New: to, Err: fs.ErrExist}
	}
	return sh.Rename(sfrom, sto)
}

func (m *SMB) Remove(ctx context.Context, p string, _ bool) error {
	sh, sp, err := m.share(ctx, p)
	if err != nil {
		return err
	}
	return sh.Remove(sp)
}
