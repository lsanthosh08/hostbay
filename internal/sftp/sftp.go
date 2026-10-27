// Package sftp is the file side of a host connection: browsing, editing and
// transfers over SFTP, and saving root-owned files through sudo (R4, R5).
package sftp

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	sftplib "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Entry is one directory listing row, shaped like the UI's DirEntry.
type Entry struct {
	Name  string
	Path  string
	IsDir bool
	Size  uint64
	Mode  uint32 // raw st_mode, file type bits included
	Mtime int64
	Owner uint32
	Group uint32
}

// Pool keeps one SFTP client per SSH connection, shared by every call on
// that connection. A client leaves the pool when its connection ends, so a
// rebuild (a new *ssh.Client) or the last lease's release retires it.
type Pool struct {
	mu     sync.Mutex
	m      map[*ssh.Client]*sftplib.Client
	noSFTP map[*ssh.Client]bool // connections whose server refused SFTP
}

// ErrNoSFTP means the server offers no SFTP subsystem, so file calls on
// that connection go over SCP instead (R38).
var ErrNoSFTP = errors.New("the server has no SFTP subsystem")

// Get returns c's SFTP client, starting one if it has none. When the server
// refuses SFTP it returns ErrNoSFTP, and keeps doing so without asking again
// until c ends.
func (p *Pool) Get(c *ssh.Client) (*Conn, error) {
	p.mu.Lock()
	s, refused := p.m[c], p.noSFTP[c]
	p.mu.Unlock()
	if refused {
		return nil, ErrNoSFTP
	}
	if s != nil {
		return &Conn{ssh: c, sftp: s}, nil
	}

	// Started outside the lock so a slow host does not stall the others.
	s, err := sftplib.NewClient(c)
	if err != nil {
		// A connection that still answers failed SFTP itself; a dropped one
		// is not a refusal and must not be remembered as one.
		if _, _, kerr := c.SendRequest("keepalive@openssh.com", true, nil); kerr == nil {
			p.refuse(c)
			return nil, fmt.Errorf("%w: %v", ErrNoSFTP, err)
		}
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if old := p.m[c]; old != nil { // another call won the race
		s.Close()
		return &Conn{ssh: c, sftp: old}, nil
	}
	if p.m == nil {
		p.m = map[*ssh.Client]*sftplib.Client{}
	}
	p.m[c] = s
	go func() {
		s.Wait()
		p.mu.Lock()
		if p.m[c] == s {
			delete(p.m, c)
		}
		p.mu.Unlock()
		s.Close()
	}()
	return &Conn{ssh: c, sftp: s}, nil
}

// refuse remembers that c's server has no SFTP until c ends.
func (p *Pool) refuse(c *ssh.Client) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.noSFTP[c] {
		return
	}
	if p.noSFTP == nil {
		p.noSFTP = map[*ssh.Client]bool{}
	}
	p.noSFTP[c] = true
	go func() {
		c.Wait()
		p.mu.Lock()
		delete(p.noSFTP, c)
		p.mu.Unlock()
	}()
}

// Conn runs file operations on one connection: SFTP where it can, and shell
// commands (cp, chmod, sudo) where SFTP has no equivalent.
type Conn struct {
	ssh  *ssh.Client
	sftp *sftplib.Client
}

// Shell returns a Conn for a server without SFTP. Only the methods that run
// shell commands work on it: Copy, Chmod, and Remove with sudo.
func Shell(c *ssh.Client) *Conn { return &Conn{ssh: c} }

// List returns dir's entries, folders first, then by case-insensitive name.
func (c *Conn) List(dir string) ([]Entry, error) {
	fis, err := c.sftp.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(fis))
	for _, fi := range fis {
		out = append(out, entry(fi.Name(), path.Join(dir, fi.Name()), fi))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func entry(name, p string, fi os.FileInfo) Entry {
	e := Entry{Name: name, Path: p, IsDir: fi.IsDir(), Size: uint64(fi.Size()), Mtime: fi.ModTime().Unix()}
	if st, ok := fi.Sys().(*sftplib.FileStat); ok {
		e.Mode, e.Owner, e.Group = st.Mode, st.UID, st.GID
	}
	return e
}

// Stat describes p, following symlinks. A missing p is fs.ErrNotExist.
func (c *Conn) Stat(p string) (Entry, error) {
	fi, err := c.sftp.Stat(p)
	if err != nil {
		return Entry{}, err
	}
	return entry(path.Base(p), p, fi), nil
}

// Open streams p's content.
func (c *Conn) Open(p string) (io.ReadCloser, error) { return c.sftp.Open(p) }

// Upload is a file being written through a temp file beside its
// destination (KTD17).
type Upload struct {
	c        *Conn
	f        *sftplib.File
	tmp, dst string
}

// Create starts replacing p through a temp file in its folder; Commit
// renames it into place, so a cancelled or failed upload leaves p as it
// was. Over an existing file the temp file takes its permission bits, and a
// symlink is followed to its target so the link stays a link.
func (c *Conn) Create(p string) (*Upload, error) {
	dst, err := c.resolve(p)
	if err != nil {
		return nil, err
	}
	var b [8]byte
	rand.Read(b[:])
	tmp := path.Join(path.Dir(dst), ".hostbay-"+hex.EncodeToString(b[:])+".part")
	f, err := c.sftp.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return nil, err
	}
	u := &Upload{c: c, f: f, tmp: tmp, dst: dst}
	if fi, err := c.sftp.Stat(dst); err == nil {
		if err := f.Chmod(fi.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)); err != nil {
			u.Abort()
			return nil, err
		}
	}
	return u, nil
}

// resolve follows p through any symlinks to the file they name.
func (c *Conn) resolve(p string) (string, error) {
	for range 40 {
		fi, err := c.sftp.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
		if err != nil {
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return p, nil
		}
		t, err := c.sftp.ReadLink(p)
		if err != nil {
			return "", err
		}
		if !path.IsAbs(t) {
			t = path.Join(path.Dir(p), t)
		}
		p = t
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", p)
}

func (u *Upload) Write(b []byte) (int, error) { return u.f.Write(b) }

// Commit renames the temp file over the destination with
// posix-rename@openssh.com, which replaces an existing file in one step.
func (u *Upload) Commit() error {
	err := u.f.Close()
	if err == nil {
		err = u.c.sftp.PosixRename(u.tmp, u.dst)
	}
	if err != nil {
		u.c.sftp.Remove(u.tmp)
	}
	return err
}

// Abort removes the temp file, leaving the destination untouched.
func (u *Upload) Abort() error {
	u.f.Close()
	return u.c.sftp.Remove(u.tmp)
}

// Read returns a file's content for the editor.
func (c *Conn) Read(p string) (string, error) {
	f, err := c.sftp.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	return string(b), err
}

// Write replaces p's content. Without sudo it writes over SFTP and, as
// files.rs did, falls back to sudo only when that is refused for permission.
func (c *Conn) Write(p, content string, sudo bool, password string) error {
	if !sudo {
		err := c.put(p, strings.NewReader(content))
		if !errors.Is(err, os.ErrPermission) {
			return err
		}
	}
	return c.sudoWrite(p, content, password)
}

// put streams r into p, truncating it first so shorter content leaves no
// old tail behind.
func (c *Conn) put(p string, r io.Reader) error {
	f, err := c.sftp.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (c *Conn) Mkdir(p string) error         { return c.sftp.Mkdir(p) }
func (c *Conn) Rename(from, to string) error { return c.sftp.Rename(from, to) }

// Remove deletes a file or an empty folder over SFTP; with sudo, or when
// SFTP is refused for permission, it runs rm (-rf for a folder) as root.
func (c *Conn) Remove(p string, isDir, sudo bool, password string) error {
	if !sudo {
		remove := c.sftp.Remove
		if isDir {
			remove = c.sftp.RemoveDirectory
		}
		if err := remove(p); !errors.Is(err, os.ErrPermission) {
			return err
		}
	}
	flag := "-f"
	if isDir {
		flag = "-rf"
	}
	return c.shell("remove", true, password, "rm", flag, "--", p)
}

// Copy runs cp -a, which SFTP has no request for.
func (c *Conn) Copy(from, to string, sudo bool, password string) error {
	return c.shell("copy", sudo, password, "cp", "-a", "--", from, to)
}

// Chmod runs chmod with mode in octal, so setuid and sticky bits pass
// through as the user typed them.
func (c *Conn) Chmod(p string, mode uint32, sudo bool, password string) error {
	return c.shell("chmod", sudo, password, "chmod", "--", fmt.Sprintf("%o", mode), p)
}

// Upload streams a local file to remote, replacing its content.
func (c *Conn) Upload(local, remote string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	return c.put(remote, f)
}

// Download streams remote into a temp file beside local and renames it into
// place, so a failed transfer never leaves a partial or clobbered file.
func (c *Conn) Download(remote, local string) error {
	src, err := c.sftp.Open(remote)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(local), ".hostbay-*.part")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // nothing left to remove after the rename
	_, err = io.Copy(tmp, src)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644) // CreateTemp makes it 0600
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), local)
}
