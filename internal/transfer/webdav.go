package transfer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"time"

	"github.com/studio-b12/gowebdav"
)

// httpClient is the client of WebDAV and S3 hosts: tlsCfg from the pin
// store (nil for http://), with dial, TLS and header timeouts but no limit on
// a whole body, so a large file takes as long as it needs. It refuses any
// request beyond origin's scheme and host, so a redirect cannot carry a
// password to another server or down to http://. dial, when set, carries
// every connection (the host's proxy).
func httpClient(tlsCfg *tls.Config, origin *url.URL, dial DialFunc) *http.Client {
	dc := (&net.Dialer{Timeout: dialTimeout}).DialContext
	if dial != nil {
		dc = func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			return dial(ctx, network, addr)
		}
	}
	return &http.Client{Transport: sameOrigin{origin: origin, rt: &http.Transport{
		DialContext:           dc,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: time.Minute,
		ForceAttemptHTTP2:     true,
	}}}
}

// HTTPClient is httpClient for the other HTTP-based hosts (KTD24): a
// Kubernetes API server's or an Elasticsearch node's requests go only
// through dial and never follow the server elsewhere. Like httpClient it
// reads no proxy from the environment.
func HTTPClient(tlsCfg *tls.Config, origin *url.URL, dial DialFunc) *http.Client {
	return httpClient(tlsCfg, origin, dial)
}

type sameOrigin struct {
	origin *url.URL
	rt     http.RoundTripper
}

// CloseIdleConnections passes http.Client.CloseIdleConnections through to
// the transport, so a closed client releases its pooled connections and
// any jump-host lease they hold.
func (s sameOrigin) CloseIdleConnections() {
	if c, ok := s.rt.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

func (s sameOrigin) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != s.origin.Scheme || r.URL.Host != s.origin.Host {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, fmt.Errorf("refusing to follow the server to %s://%s", r.URL.Scheme, r.URL.Host)
	}
	return s.rt.RoundTrip(r)
}

// httpURL checks a WebDAV or S3 URL: https needs tlsCfg, http must have none.
func httpURL(raw string, tlsCfg *tls.Config) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		return nil, errors.New("enter the server's http(s) URL")
	case (u.Scheme == "https") != (tlsCfg != nil):
		return nil, errors.New("an https URL needs TLS and an http URL has none")
	}
	return u, nil
}

// WebDAVConfig says how to reach a WebDAV folder (R41).
type WebDAVConfig struct {
	URL            string // the folder that "/" stands for
	User, Password string
	TLS            *tls.Config // from the pin store for https; nil for http
	Dial           DialFunc
}

// WebDAV is a Source on a WebDAV server. Writes go to a temporary name and
// commit with MOVE and Overwrite: T, which replaces the file in one step.
type WebDAV struct{ c *gowebdav.Client }

// DialWebDAV checks the folder answers, so a wrong password or a refused
// certificate shows at once, and returns the source.
func DialWebDAV(ctx context.Context, cfg WebDAVConfig) (*WebDAV, error) {
	u, err := httpURL(cfg.URL, cfg.TLS)
	if err != nil {
		return nil, err
	}
	// Preemptive Basic auth: gowebdav's negotiating authorizer keeps a copy
	// of every upload in memory to resend it after a 401.
	c := gowebdav.NewAuthClient(u.String(), gowebdav.NewPreemptiveAuth(davBasic{cfg.User, cfg.Password}))
	c.SetTransport(httpClient(cfg.TLS, u, cfg.Dial).Transport)
	d := &WebDAV{c: c}
	if _, err := d.Stat(ctx, "/"); err != nil {
		return nil, err
	}
	return d, nil
}

// davBasic sends Basic credentials, if there are any, with every request.
type davBasic struct{ user, pw string }

func (b davBasic) Authorize(_ *http.Client, rq *http.Request, _ string) error {
	if b.user != "" || b.pw != "" {
		rq.SetBasicAuth(b.user, b.pw)
	}
	return nil
}

func (b davBasic) Verify(_ *http.Client, rs *http.Response, p string) (bool, error) {
	if rs.StatusCode == http.StatusUnauthorized {
		return false, gowebdav.NewPathError("Authorize", p, rs.StatusCode)
	}
	return false, nil
}

func (b davBasic) Close() error                  { return nil }
func (b davBasic) Clone() gowebdav.Authenticator { return b }
func (b davBasic) String() string                { return "Basic" }

func (d *WebDAV) List(_ context.Context, dir string) ([]Entry, error) {
	fis, err := d.c.ReadDir(dir)
	if err != nil {
		return nil, davErr(err)
	}
	out := make([]Entry, 0, len(fis))
	for _, fi := range fis {
		out = append(out, davEntry(fi.Name(), joinRaw(dir, fi.Name()), fi))
	}
	sortEntries(out)
	return out, nil
}

func (d *WebDAV) Stat(_ context.Context, p string) (Entry, error) {
	fi, err := d.c.Stat(p)
	if err != nil {
		return Entry{}, davErr(err)
	}
	return davEntry(path.Base(p), p, fi), nil
}

func (d *WebDAV) Open(_ context.Context, p string) (io.ReadCloser, error) {
	r, err := d.c.ReadStream(p)
	return r, davErr(err)
}

func (d *WebDAV) Create(_ context.Context, dir, rel string, size int64) (Writer, error) {
	dst := path.Join(dir, rel)
	tmp := path.Join(path.Dir(dst), tempName())
	pr, pw := io.Pipe()
	w := &davWriter{d: d, pw: pw, tmp: tmp, dst: dst, done: make(chan error, 1)}
	go func() {
		err := d.c.WriteStreamWithLength(tmp, pr, size, 0)
		pr.CloseWithError(errors.Join(err, io.ErrClosedPipe)) // a refused upload stops the writes
		w.done <- err
	}()
	return w, nil
}

type davWriter struct {
	d        *WebDAV
	pw       *io.PipeWriter
	done     chan error
	tmp, dst string
}

func (w *davWriter) Write(b []byte) (int, error) { return w.pw.Write(b) }

func (w *davWriter) Commit() error {
	w.pw.Close()
	err := <-w.done
	if err == nil {
		err = w.d.c.Rename(w.tmp, w.dst, true)
	}
	if err != nil {
		w.cleanup()
	}
	return davErr(err)
}

func (w *davWriter) Abort() error {
	w.pw.CloseWithError(errAborted)
	<-w.done
	return w.cleanup()
}

// How long cleanup keeps at a temporary file: tempCleanup bounds getting a
// DELETE through, then tempSettle is how long it keeps checking the file
// stays gone.
const (
	tempCleanup = 30 * time.Second
	tempSettle  = 15 * time.Second
)

// cleanup deletes the temporary file of an upload that did not commit. A
// slow server may still be finishing the cut-off PUT: it can refuse the
// DELETE for a while (423, 5xx, a dropped connection), or write the file
// only after a DELETE found nothing. So cleanup retries any failure with
// backoff until a DELETE goes through, then keeps checking in the
// background and deletes the file again if it reappears. None of it uses
// the transfer's context, which a cancel has already ended.
func (w *davWriter) cleanup() error {
	deadline := time.Now().Add(tempCleanup)
	wait := 50 * time.Millisecond
	backoff := func() {
		time.Sleep(wait)
		wait = min(wait*2, 2*time.Second)
	}
	err := w.d.c.Remove(w.tmp) // a 404 counts as removed
	for err != nil && time.Now().Before(deadline) {
		backoff()
		err = w.d.c.Remove(w.tmp)
	}
	if err != nil {
		return davErr(err)
	}
	go func() {
		wait = 250 * time.Millisecond
		for end := time.Now().Add(tempSettle); time.Now().Before(end); {
			backoff()
			if _, err := w.d.c.Stat(w.tmp); !gowebdav.IsErrNotFound(err) {
				w.d.c.Remove(w.tmp) // back, or the check failed: delete regardless
			}
		}
	}()
	return nil
}

func (d *WebDAV) Mkdir(_ context.Context, dir, rel string) error {
	return davErr(d.c.Mkdir(path.Join(dir, rel), 0))
}

func (d *WebDAV) Rename(_ context.Context, from, to string) error {
	return davErr(d.c.Rename(from, to, false))
}

// Remove deletes a file, or a folder only when it is empty: WebDAV's DELETE
// of a folder takes everything in it.
func (d *WebDAV) Remove(_ context.Context, p string, isDir bool) error {
	if isDir {
		fis, err := d.c.ReadDir(p)
		if err != nil {
			return davErr(err)
		}
		if len(fis) > 0 {
			return fmt.Errorf("%s: the folder is not empty", p)
		}
	}
	return davErr(d.c.Remove(p))
}

func davErr(err error) error {
	if gowebdav.IsErrNotFound(err) {
		return fmt.Errorf("%w: %w", fs.ErrNotExist, err)
	}
	return err
}

func davEntry(name, p string, fi os.FileInfo) Entry {
	mode := uint32(0o100644)
	if fi.IsDir() {
		mode = modeDir | 0o755
	}
	return Entry{Name: name, Path: p, IsDir: fi.IsDir(), Size: uint64(max(fi.Size(), 0)), Mode: mode, Mtime: fi.ModTime().Unix()}
}
