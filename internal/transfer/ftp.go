package transfer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/textproto"
	"path"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
)

// dialTimeout bounds connecting and signing in to a file server.
const dialTimeout = 30 * time.Second

// DialFunc opens a TCP connection. Hostbay passes one that goes through
// the host's proxy (KTD22); nil dials directly.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// FTPConfig says how to reach an FTP or FTPS server (R39).
type FTPConfig struct {
	Addr           string // host:port
	User, Password string // an empty User signs in as anonymous
	// TLS is nil for plain FTP. With it the connection is FTPS and never
	// falls back to plain FTP: a server that refuses TLS is an error.
	TLS      *tls.Config
	Implicit bool     // TLS from the first byte (port 990), not AUTH TLS
	Dial     DialFunc // the control and every data connection
}

// FTP is a Source on an FTP or FTPS server. A control connection moves one
// file at a time, so a file being read or written holds a connection of its
// own; connections wait in a pool between calls. Uploads go to a temporary
// name; FTP has no rename that is sure to replace a file (KTD17), so an
// overwrite that the server will not rename over removes the old file first.
type FTP struct {
	cfg FTPConfig

	mu   sync.Mutex
	idle []*ftp.ServerConn
}

// DialFTP signs in once, so a wrong password or a refused certificate shows
// at once, and returns the source.
func DialFTP(ctx context.Context, cfg FTPConfig) (*FTP, error) {
	f := &FTP{cfg: cfg}
	c, err := f.dial(ctx)
	if err != nil {
		return nil, err
	}
	f.put(c)
	return f, nil
}

func (f *FTP) dial(ctx context.Context) (*ftp.ServerConn, error) {
	opts := []ftp.DialOption{ftp.DialWithContext(ctx), ftp.DialWithTimeout(dialTimeout)}
	switch {
	case f.cfg.TLS != nil && f.cfg.Implicit:
		opts = append(opts, ftp.DialWithTLS(f.cfg.TLS))
	case f.cfg.TLS != nil:
		opts = append(opts, ftp.DialWithExplicitTLS(f.cfg.TLS))
	}
	if f.cfg.Dial != nil {
		opts = append(opts, ftp.DialWithDialFunc(f.dialFunc(ctx)))
	}
	c, err := ftp.Dial(f.cfg.Addr, opts...)
	if err != nil {
		return nil, err
	}
	user, pw := f.cfg.User, f.cfg.Password
	if user == "" {
		user, pw = "anonymous", "anonymous"
	}
	// With explicit TLS the handshake runs before USER is written, so a
	// refused certificate stops the sign-in before it starts.
	if err := c.Login(user, pw); err != nil {
		c.Quit()
		return nil, err
	}
	return c, nil
}

// dialFunc dials one control connection and its data connections through
// cfg.Dial. With a dial function of its own the library leaves TLS to it,
// so implicit FTPS's control connection and every FTPS data connection are
// wrapped here, never left plain. The control connection's peer is the
// proxy, so a data address the library builds from it goes to the
// server's own name instead.
func (f *FTP) dialFunc(ctx context.Context) func(network, addr string) (net.Conn, error) {
	server, _, _ := net.SplitHostPort(f.cfg.Addr)
	var peer string // the control connection's remote IP, once dialled
	return func(network, addr string) (net.Conn, error) {
		control := peer == ""
		dctx := ctx
		if !control {
			dctx = context.Background() // data connections outlive the sign-in's context
			if h, p, err := net.SplitHostPort(addr); err == nil && h == peer {
				addr = net.JoinHostPort(server, p)
			}
		}
		dctx, cancel := context.WithTimeout(dctx, dialTimeout)
		defer cancel()
		c, err := f.cfg.Dial(dctx, network, addr)
		if err != nil {
			return nil, err
		}
		ta, ok := c.RemoteAddr().(*net.TCPAddr) // the library needs a TCP address
		if !ok {
			c.Close()
			return nil, errors.New("the connection has no TCP address")
		}
		if control {
			peer = ta.IP.String()
			if f.cfg.TLS == nil || !f.cfg.Implicit {
				return c, nil // explicit TLS: the library upgrades after AUTH TLS
			}
			tc := tls.Client(c, f.cfg.TLS)
			if err := tc.HandshakeContext(dctx); err != nil {
				c.Close()
				return nil, err
			}
			return tc, nil
		}
		if f.cfg.TLS != nil {
			return tls.Client(c, f.cfg.TLS), nil // the handshake runs on first use, as the library's own
		}
		return c, nil
	}
}

// conn takes an idle connection that still answers, or dials a new one.
func (f *FTP) conn(ctx context.Context) (*ftp.ServerConn, error) {
	for {
		f.mu.Lock()
		if len(f.idle) == 0 {
			f.mu.Unlock()
			return f.dial(ctx)
		}
		c := f.idle[len(f.idle)-1]
		f.idle = f.idle[:len(f.idle)-1]
		f.mu.Unlock()
		if c.NoOp() == nil {
			return c, nil
		}
		c.Quit() // timed out on the server while idle
	}
}

func (f *FTP) put(c *ftp.ServerConn) {
	f.mu.Lock()
	f.idle = append(f.idle, c)
	f.mu.Unlock()
}

// release returns c to the pool unless err left it out of step: a server
// reply keeps the connection usable, a network error does not.
func (f *FTP) release(c *ftp.ServerConn, err error) {
	var te *textproto.Error
	if err == nil || errors.As(err, &te) {
		f.put(c)
	} else {
		c.Quit()
	}
}

func (f *FTP) do(ctx context.Context, fn func(*ftp.ServerConn) error) error {
	c, err := f.conn(ctx)
	if err != nil {
		return err
	}
	err = fn(c)
	f.release(c, err)
	return err
}

// Close signs out of every idle connection.
func (f *FTP) Close() error {
	f.mu.Lock()
	idle := f.idle
	f.idle = nil
	f.mu.Unlock()
	for _, c := range idle {
		c.Quit()
	}
	return nil
}

func (f *FTP) Limit() int { return 1 }

func (f *FTP) List(ctx context.Context, dir string) ([]Entry, error) {
	var out []Entry
	err := f.do(ctx, func(c *ftp.ServerConn) error {
		es, err := c.List(dir)
		for _, e := range es {
			if e.Name != "." && e.Name != ".." {
				out = append(out, ftpEntry(e.Name, joinRaw(dir, e.Name), e))
			}
		}
		return ftpErr(err)
	})
	sortEntries(out)
	return out, err
}

func (f *FTP) Stat(ctx context.Context, p string) (Entry, error) {
	var out Entry
	err := f.do(ctx, func(c *ftp.ServerConn) (err error) {
		out, err = ftpStat(c, p)
		return err
	})
	return out, err
}

// ftpStat asks with MLST, or lists the parent folder on a server without it.
func ftpStat(c *ftp.ServerConn, p string) (Entry, error) {
	e, err := c.GetEntry(p)
	var te *textproto.Error
	if errors.As(err, &te) && te.Code == ftp.StatusNotImplemented {
		es, lerr := c.List(path.Dir(p))
		if lerr != nil {
			return Entry{}, ftpErr(lerr)
		}
		err = fmt.Errorf("%s: %w", p, fs.ErrNotExist)
		for _, le := range es {
			if le.Name == path.Base(p) {
				e, err = le, nil
			}
		}
	}
	if err != nil {
		return Entry{}, ftpErr(err)
	}
	return ftpEntry(path.Base(p), p, e), nil
}

func (f *FTP) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	c, err := f.conn(ctx)
	if err != nil {
		return nil, err
	}
	r, err := c.Retr(p)
	if err != nil {
		f.release(c, err)
		return nil, ftpErr(err)
	}
	return &ftpReader{Response: r, f: f, c: c}, nil
}

// ftpReader gives its connection back when the download is closed.
type ftpReader struct {
	*ftp.Response
	f *FTP
	c *ftp.ServerConn
}

func (r *ftpReader) Close() error {
	err := r.Response.Close()
	r.f.release(r.c, err)
	return err
}

func (f *FTP) Create(ctx context.Context, dir, rel string, _ int64) (Writer, error) {
	c, err := f.conn(ctx)
	if err != nil {
		return nil, err
	}
	dst := path.Join(dir, rel)
	w := &ftpWriter{f: f, c: c, dst: dst, tmp: path.Join(path.Dir(dst), tempName()), done: make(chan error, 1)}
	if _, err := ftpStat(c, dst); err == nil {
		w.exists = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		f.release(c, err)
		return nil, err
	}
	pr, pw := io.Pipe()
	w.pw = pw
	go func() {
		err := c.Stor(w.tmp, pr)
		pr.CloseWithError(errors.Join(err, io.ErrClosedPipe)) // a refused upload stops the writes
		w.done <- err
	}()
	return w, nil
}

// ftpWriter streams one STOR to a temporary name, renamed into place on
// Commit, so an aborted overwrite leaves the original as it was.
type ftpWriter struct {
	f        *FTP
	c        *ftp.ServerConn
	pw       *io.PipeWriter
	done     chan error
	tmp, dst string
	exists   bool // dst was there before the upload
}

func (w *ftpWriter) Write(b []byte) (int, error) { return w.pw.Write(b) }

func (w *ftpWriter) Commit() error {
	w.pw.Close()
	err := <-w.done
	if err == nil {
		err = w.c.Rename(w.tmp, w.dst)
		if err != nil && w.exists {
			// Some servers do not rename over a file: remove it first, and
			// once it is gone keep the upload rather than lose both.
			if err = w.c.Delete(w.dst); err == nil {
				if err = w.c.Rename(w.tmp, w.dst); err != nil {
					w.f.release(w.c, err)
					return fmt.Errorf("the upload is kept as %s: %w", w.tmp, ftpErr(err))
				}
			}
		}
	}
	if err != nil {
		w.c.Delete(w.tmp)
	}
	w.f.release(w.c, err)
	return ftpErr(err)
}

var errAborted = errors.New("transfer aborted")

func (w *ftpWriter) Abort() error {
	w.pw.CloseWithError(errAborted)
	<-w.done // the server keeps what arrived: remove it
	err := w.c.Delete(w.tmp)
	w.f.release(w.c, err)
	return ftpErr(err)
}

func (f *FTP) Mkdir(ctx context.Context, dir, rel string) error {
	return f.do(ctx, func(c *ftp.ServerConn) error { return ftpErr(c.MakeDir(path.Join(dir, rel))) })
}

func (f *FTP) Rename(ctx context.Context, from, to string) error {
	return f.do(ctx, func(c *ftp.ServerConn) error { return ftpErr(c.Rename(from, to)) })
}

func (f *FTP) Remove(ctx context.Context, p string, isDir bool) error {
	return f.do(ctx, func(c *ftp.ServerConn) error {
		if isDir {
			return ftpErr(c.RemoveDir(p))
		}
		return ftpErr(c.Delete(p))
	})
}

// ftpErr reads FTP's "file unavailable" reply as fs.ErrNotExist, keeping
// the server's message.
func ftpErr(err error) error {
	var te *textproto.Error
	if errors.As(err, &te) && te.Code == ftp.StatusFileUnavailable {
		return fmt.Errorf("%w: %w", fs.ErrNotExist, err)
	}
	return err
}

func ftpEntry(name, p string, e *ftp.Entry) Entry {
	mode := uint32(0o100644)
	switch e.Type {
	case ftp.EntryTypeFolder:
		mode = modeDir | 0o755
	case ftp.EntryTypeLink:
		mode = modeSymlink | 0o777
	}
	return Entry{Name: name, Path: p, IsDir: e.Type == ftp.EntryTypeFolder, Size: e.Size, Mode: mode, Mtime: e.Time.Unix()}
}

// joinRaw joins a listed name to its folder without cleaning it, so a name
// like ".." still reaches the engine's check as the server sent it.
func joinRaw(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}
