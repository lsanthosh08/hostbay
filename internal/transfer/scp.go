package transfer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/admin"
	"hostbay/internal/sftp"
)

// SCP is a Source for servers without SFTP (R38). It speaks OpenSSH's scp
// protocol to `scp -f` and `scp -t` on the host's own connection, and lists
// and changes folders with ls, mkdir, mv and rm, each path quoted as one
// shell word.
type SCP struct{ c *ssh.Client }

// NewSCP wraps c. Sources over the same connection compare equal, so they
// share one concurrency limit.
func NewSCP(c *ssh.Client) SCP { return SCP{c: c} }

// Limit keeps SCP to two files at once: each holds a session channel, and
// OpenSSH allows ten per connection, shared with the host's terminals.
func (SCP) Limit() int { return 2 }

func (s SCP) List(ctx context.Context, dir string) ([]Entry, error) {
	// -H lists the folder a symlinked dir points at; -q turns control
	// characters, newlines included, into ? so each entry stays one line.
	out, err := s.run(ctx, "LC_ALL=C ls -lanqH -- "+sftp.Quote(dir))
	if err != nil {
		return nil, err
	}
	var es []Entry
	for _, line := range strings.Split(out, "\n") {
		e, ok := parseLs(line)
		if !ok || e.Name == "." || e.Name == ".." {
			continue
		}
		e.Path = path.Join(dir, e.Name)
		es = append(es, e)
	}
	sortEntries(es)
	return es, nil
}

func (s SCP) Stat(ctx context.Context, p string) (Entry, error) {
	out, err := s.run(ctx, "LC_ALL=C ls -landqL -- "+sftp.Quote(p))
	if err != nil {
		return Entry{}, err
	}
	e, ok := parseLs(strings.TrimSuffix(out, "\n"))
	if !ok {
		return Entry{}, fmt.Errorf("cannot read the listing of %s", p)
	}
	e.Name, e.Path = path.Base(p), p
	return e, nil
}

func (s SCP) Mkdir(ctx context.Context, dir, rel string) error {
	_, err := s.run(ctx, "mkdir -- "+sftp.Quote(path.Join(dir, rel)))
	return err
}

func (s SCP) Rename(ctx context.Context, from, to string) error {
	_, err := s.run(ctx, "mv -- "+sftp.Quote(from)+" "+sftp.Quote(to))
	return err
}

func (s SCP) Remove(ctx context.Context, p string, isDir bool) error {
	cmd := "rm -f -- "
	if isDir {
		cmd = "rmdir -- "
	}
	_, err := s.run(ctx, cmd+sftp.Quote(p))
	return err
}

// run runs cmd and returns its output; a failure carries stderr, and one
// that says the path is missing matches fs.ErrNotExist.
func (s SCP) run(ctx context.Context, cmd string) (string, error) {
	r, err := admin.Exec(ctx, s.c, cmd)
	if err != nil {
		return "", err
	}
	if r.Code != nil && *r.Code == 0 {
		return r.Stdout, nil
	}
	msg := strings.TrimSpace(r.Stderr)
	if msg == "" {
		msg = "command failed: " + cmd
	}
	if strings.Contains(msg, "No such file") {
		return "", fmt.Errorf("%s: %w", msg, fs.ErrNotExist)
	}
	return "", errors.New(msg)
}

// Open fetches p with `scp -f`. It accepts only a single file whose name is
// one plain path element, so a hostile server cannot redirect the write.
func (s SCP) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	c, err := s.start(ctx, "scp -f -- "+sftp.Quote(p))
	if err != nil {
		return nil, err
	}
	size, err := c.receive()
	if err != nil {
		c.close()
		return nil, err
	}
	return &scpReader{scpConn: c, left: size}, nil
}

// Create sends the file with `scp -t`. A new file goes to a temp name that
// mv renames into place on Commit. scp has no rename that replaces a file,
// so an overwrite writes in place (KTD17), which keeps the file's mode and
// writes through a symlink to its target.
func (s SCP) Create(ctx context.Context, dir, rel string, size int64) (Writer, error) {
	dst := path.Join(dir, rel)
	if !validName(path.Base(dst)) {
		return nil, fmt.Errorf("%q is not a name scp can send", path.Base(dst))
	}
	w := &scpWriter{s: s, ctx: ctx, dst: dst, target: dst, size: size}
	switch e, err := s.Stat(ctx, dst); {
	case err == nil && e.IsDir:
		return nil, fmt.Errorf("%s is a folder", dst)
	case errors.Is(err, fs.ErrNotExist):
		w.target, w.tmp = path.Join(path.Dir(dst), tempName()), true
	case err != nil:
		return nil, err
	}
	c, err := s.start(ctx, "scp -t -- "+sftp.Quote(w.target))
	if err != nil {
		return nil, err
	}
	w.scpConn = c
	err = c.ack()
	if err == nil {
		_, err = fmt.Fprintf(c.in, "C0644 %d %s\n", size, path.Base(w.target))
	}
	if err == nil {
		err = c.ack()
	}
	if err != nil {
		c.close()
		return nil, err
	}
	return w, nil
}

// scpConn is one running scp on the server.
type scpConn struct {
	sess *ssh.Session
	in   io.WriteCloser
	out  *bufio.Reader
	stop func() bool
}

func (s SCP) start(ctx context.Context, cmd string) (*scpConn, error) {
	sess, err := s.c.NewSession()
	if err != nil {
		return nil, err
	}
	in, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	out, err := sess.StdoutPipe()
	if err == nil {
		err = sess.Start(cmd)
	}
	if err != nil {
		sess.Close()
		return nil, err
	}
	return &scpConn{sess: sess, in: in, out: bufio.NewReader(out), stop: context.AfterFunc(ctx, func() { sess.Close() })}, nil
}

func (c *scpConn) close() {
	c.stop()
	c.sess.Close()
}

// ack reads the reply to the last message: 0, or 1 or 2 and an error line.
func (c *scpConn) ack() error {
	b, err := c.out.ReadByte()
	switch {
	case err != nil:
		return fmt.Errorf("scp ended early: %w", err)
	case b == 0:
		return nil
	case b == 1 || b == 2:
		msg, _ := c.out.ReadString('\n')
		return errors.New(strings.TrimSpace(msg))
	}
	return fmt.Errorf("scp: unexpected reply %q", b)
}

// receive asks for the file, reads its C directive ("C0644 <size> <name>")
// and returns the size.
func (c *scpConn) receive() (int64, error) {
	if _, err := c.in.Write([]byte{0}); err != nil {
		return 0, err
	}
	line, err := c.out.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("scp ended early: %w", err)
	}
	line = strings.TrimSuffix(line, "\n")
	if line == "" {
		return 0, errors.New("scp: the server sent an empty line")
	}
	if line[0] == 1 || line[0] == 2 {
		return 0, errors.New(strings.TrimSpace(line[1:]))
	}
	f := strings.SplitN(line, " ", 3)
	if line[0] != 'C' || len(f) != 3 {
		return 0, fmt.Errorf("scp: expected one file, got %q", line)
	}
	size, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("scp: bad size in %q", line)
	}
	if !validName(f[2]) {
		return 0, fmt.Errorf("scp: refusing the file name %q the server sent", f[2])
	}
	_, err = c.in.Write([]byte{0})
	return size, err
}

type scpReader struct {
	*scpConn
	left int64
	err  error // set once the file's bytes are all read
}

func (r *scpReader) Read(b []byte) (int, error) {
	if r.left == 0 {
		if r.err == nil {
			// The sender ends the file with its status; acknowledge it.
			r.err = r.ack()
			if r.err == nil {
				_, r.err = r.in.Write([]byte{0})
			}
			if r.err == nil {
				r.err = io.EOF
			}
		}
		return 0, r.err
	}
	if int64(len(b)) > r.left {
		b = b[:r.left]
	}
	n, err := r.out.Read(b)
	r.left -= int64(n)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *scpReader) Close() error {
	r.close()
	return nil
}

type scpWriter struct {
	*scpConn
	s           SCP
	ctx         context.Context
	dst, target string
	tmp         bool // target is a temp file to rename onto dst
	size, n     int64
}

func (w *scpWriter) Write(b []byte) (int, error) {
	if w.n+int64(len(b)) > w.size {
		return 0, fmt.Errorf("%s: more than the %d bytes announced", w.dst, w.size)
	}
	n, err := w.in.Write(b)
	w.n += int64(n)
	return n, err
}

func (w *scpWriter) Commit() error {
	err := w.end()
	if err == nil && w.tmp {
		_, err = w.s.run(w.ctx, "mv -f -- "+sftp.Quote(w.target)+" "+sftp.Quote(w.dst))
	}
	if err != nil {
		w.removeTemp()
	}
	return err
}

// end finishes the file and waits for scp to exit.
func (w *scpWriter) end() error {
	defer w.close()
	if w.n != w.size {
		return fmt.Errorf("%s: %d of %d bytes sent", w.dst, w.n, w.size)
	}
	if _, err := w.in.Write([]byte{0}); err != nil {
		return err
	}
	if err := w.ack(); err != nil {
		return err
	}
	w.in.Close()
	return w.sess.Wait()
}

func (w *scpWriter) Abort() error {
	w.close()
	return w.removeTemp()
}

// removeTemp deletes the temp file even after a cancel.
func (w *scpWriter) removeTemp() error {
	if !w.tmp {
		return nil
	}
	_, err := w.s.run(context.WithoutCancel(w.ctx), "rm -f -- "+sftp.Quote(w.target))
	return err
}

// parseLs reads one line of `ls -ln`: mode, links, uid, gid, size, three
// date fields, then the name. Lines for anything but files, folders and
// symlinks, and the "total" line, are not entries.
func parseLs(line string) (Entry, bool) {
	f, name, ok := lsFields(line, 8)
	if !ok {
		return Entry{}, false
	}
	mode, ok := lsMode(f[0])
	if !ok {
		return Entry{}, false
	}
	size, err := strconv.ParseUint(f[4], 10, 64)
	if err != nil {
		return Entry{}, false
	}
	uid, _ := strconv.ParseUint(f[2], 10, 32)
	gid, _ := strconv.ParseUint(f[3], 10, 32)
	if mode&modeType == modeSymlink {
		name, _, _ = strings.Cut(name, " -> ")
	}
	return Entry{Name: name, IsDir: mode&modeType == modeDir, Size: size, Mode: mode,
		Mtime: lsTime(f[5], f[6], f[7]), Owner: uint32(uid), Group: uint32(gid)}, true
}

// lsFields splits off n space-separated fields and returns the rest of the
// line after the single space that follows them, so a name keeps its own
// spaces.
func lsFields(line string, n int) ([]string, string, bool) {
	var f []string
	s := line
	for range n {
		s = strings.TrimLeft(s, " ")
		i := strings.IndexByte(s, ' ')
		if i <= 0 {
			return nil, "", false
		}
		f = append(f, s[:i])
		s = s[i:]
	}
	if len(s) < 2 {
		return nil, "", false
	}
	return f, s[1:], true
}

// lsSpecial is the setuid, setgid or sticky bit an s or t in that
// permission column stands for.
var lsSpecial = [9]uint32{2: 0o4000, 5: 0o2000, 8: 0o1000}

// lsMode turns "-rwsr-xr-t" into a raw st_mode.
func lsMode(s string) (uint32, bool) {
	if len(s) < 10 {
		return 0, false
	}
	var m uint32
	switch s[0] {
	case '-':
		m = 0o100000
	case 'd':
		m = modeDir
	case 'l':
		m = modeSymlink
	default:
		return 0, false
	}
	for i := range 9 {
		bit := uint32(0o400) >> i
		switch s[1+i] {
		case 'r', 'w', 'x':
			m |= bit
		case 's', 't': // setuid, setgid or sticky, with execute
			m |= bit | lsSpecial[i]
		case 'S', 'T':
			m |= lsSpecial[i]
		}
	}
	return m, true
}

// lsTime reads ls's "Oct 23 10:00" (within the last six months) or
// "Oct 23 2025" in the local time zone, as near as ls lets it.
func lsTime(mon, day, yt string) int64 {
	if strings.Contains(yt, ":") {
		now := time.Now()
		t, err := time.ParseInLocation("Jan 2 15:04 2006", fmt.Sprintf("%s %s %s %d", mon, day, yt, now.Year()), time.Local)
		if err != nil {
			return 0
		}
		if t.After(now.AddDate(0, 0, 1)) {
			t = t.AddDate(-1, 0, 0)
		}
		return t.Unix()
	}
	t, err := time.ParseInLocation("Jan 2 2006", mon+" "+day+" "+yt, time.Local)
	if err != nil {
		return 0
	}
	return t.Unix()
}
