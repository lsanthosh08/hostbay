package transfer

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"hostbay/internal/s3sig"
)

// S3Config says how to reach a bucket on an S3-compatible store (R42).
type S3Config struct {
	Endpoint       string // the service's http(s) URL
	Bucket, Region string
	PathStyle      bool
	Access, Secret string
	TLS            *tls.Config // from the pin store for https; nil for http
	Dial           DialFunc
}

// S3 is a Source on one bucket. Folders are key prefixes ending in "/",
// and an empty folder is a zero-byte "name/" marker. A PUT and a multipart
// Complete are already atomic, so nothing needs a temporary name.
type S3 struct{ b *s3sig.Bucket }

// DialS3 lists the bucket's top level once, so wrong keys or a refused
// certificate show at once, and returns the source.
func DialS3(ctx context.Context, cfg S3Config) (*S3, error) {
	u, err := httpURL(strings.TrimRight(cfg.Endpoint, "/"), cfg.TLS)
	if err != nil {
		return nil, err
	}
	if cfg.Bucket == "" || cfg.Access == "" || cfg.Secret == "" {
		return nil, errors.New("S3 needs a bucket, an access key and a secret key")
	}
	b := &s3sig.Bucket{Endpoint: u, Name: cfg.Bucket, PathStyle: cfg.PathStyle, Access: cfg.Access, Secret: cfg.Secret, Region: cfg.Region}
	b.Client = httpClient(cfg.TLS, b.URL(""), cfg.Dial)
	s := &S3{b: b}
	if _, err := s.list(ctx, "", "/", 1); err != nil {
		return nil, err
	}
	return s, nil
}

// key turns a source path into an object key.
func key(p string) string { return strings.TrimPrefix(p, "/") }

// do sends a request with body, all of it signed.
func (s *S3) do(ctx context.Context, method, k string, q url.Values, body []byte, h http.Header) (*http.Response, error) {
	res, err := s.b.Do(ctx, method, k, q, body, h)
	return reply(method, k, res, err)
}

// reply turns an S3 error reply into an error; 404 is fs.ErrNotExist.
func reply(method, k string, res *http.Response, err error) (*http.Response, error) {
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		err := fmt.Errorf("S3 %s %s: %s %s", method, k, res.Status, s3Message(msg))
		if res.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%w: %w", fs.ErrNotExist, err)
		}
		return nil, err
	}
	return res, nil
}

// s3Message is the Message of an S3 error document, if it is one.
func s3Message(b []byte) string {
	var e struct{ Code, Message string }
	if xml.Unmarshal(b, &e) == nil && e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return ""
}

type listing struct {
	Contents []struct {
		Key          string
		Size         uint64
		LastModified time.Time
	}
	CommonPrefixes []struct{ Prefix string }
	IsTruncated    bool
	NextToken      string `xml:"NextContinuationToken"`
}

// list runs ListObjectsV2 under prefix, page after page; max > 0 stops
// after one page of at most max keys.
func (s *S3) list(ctx context.Context, prefix, delim string, max int) (listing, error) {
	var all listing
	q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
	if delim != "" {
		q.Set("delimiter", delim)
	}
	if max > 0 {
		q.Set("max-keys", strconv.Itoa(max))
	}
	for {
		res, err := s.do(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return all, err
		}
		var page listing
		err = xml.NewDecoder(res.Body).Decode(&page)
		res.Body.Close()
		if err != nil {
			return all, fmt.Errorf("S3 listing: %w", err)
		}
		all.Contents = append(all.Contents, page.Contents...)
		all.CommonPrefixes = append(all.CommonPrefixes, page.CommonPrefixes...)
		if !page.IsTruncated || page.NextToken == "" || max > 0 {
			return all, nil
		}
		q.Set("continuation-token", page.NextToken)
	}
}

func dirPrefix(dir string) string {
	if k := key(dir); k != "" {
		return strings.TrimSuffix(k, "/") + "/"
	}
	return ""
}

// List shows names as their keys have them. A key such as "a/../../x"
// lists as a folder named "..", which the engine refuses to copy.
func (s *S3) List(ctx context.Context, dir string) ([]Entry, error) {
	prefix := dirPrefix(dir)
	l, err := s.list(ctx, prefix, "/", 0)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, p := range l.CommonPrefixes {
		name := strings.TrimSuffix(strings.TrimPrefix(p.Prefix, prefix), "/")
		out = append(out, Entry{Name: name, Path: joinRaw(dir, name), IsDir: true, Mode: modeDir | 0o755})
	}
	for _, c := range l.Contents {
		if name := strings.TrimPrefix(c.Key, prefix); name != "" { // "" is the folder's own marker
			out = append(out, Entry{Name: name, Path: joinRaw(dir, name), Size: c.Size, Mode: 0o100644, Mtime: c.LastModified.Unix()})
		}
	}
	sortEntries(out)
	return out, nil
}

func (s *S3) Stat(ctx context.Context, p string) (Entry, error) {
	k := key(p)
	if k == "" {
		return Entry{Name: "/", Path: p, IsDir: true, Mode: modeDir | 0o755}, nil
	}
	res, err := s.do(ctx, http.MethodHead, k, nil, nil, nil)
	if err == nil {
		res.Body.Close()
		mtime, _ := http.ParseTime(res.Header.Get("Last-Modified"))
		return Entry{Name: path.Base(p), Path: p, Size: uint64(max(res.ContentLength, 0)), Mode: 0o100644, Mtime: mtime.Unix()}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Entry{}, err
	}
	// Not an object: a folder if any key starts with its prefix.
	l, lerr := s.list(ctx, dirPrefix(p), "/", 1)
	if lerr != nil {
		return Entry{}, lerr
	}
	if len(l.Contents)+len(l.CommonPrefixes) == 0 {
		return Entry{}, err
	}
	return Entry{Name: path.Base(p), Path: p, IsDir: true, Mode: modeDir | 0o755}, nil
}

func (s *S3) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	res, err := s.do(ctx, http.MethodGet, key(p), nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// minPart is the part size, above S3's 5 MiB smallest part; a file too big
// for 10,000 such parts gets bigger ones.
const (
	minPart  = 8 << 20
	maxParts = 10000
)

// Create writes a file of up to one part with one PUT on Commit: once a
// server has every byte the object exists, so a cut-off copy must not send
// them all before then. A larger file streams, without being held in
// memory, as a multipart upload whose parts are sent in order; Commit
// completes it, and Abort or any failure aborts it so no parts are left.
func (s *S3) Create(ctx context.Context, dir, rel string, size int64) (Writer, error) {
	w := &s3Writer{s: s, ctx: ctx, key: key(path.Join(dir, rel)), size: size, part: max(minPart, (size+maxParts-1)/maxParts)}
	if size > w.part {
		res, err := s.do(ctx, http.MethodPost, w.key, url.Values{"uploads": {""}}, nil, nil)
		if err != nil {
			return nil, err
		}
		var r struct{ UploadId string }
		err = xml.NewDecoder(res.Body).Decode(&r)
		res.Body.Close()
		if err != nil || r.UploadId == "" {
			return nil, fmt.Errorf("S3 did not start the upload: %v", err)
		}
		w.upload = r.UploadId
	}
	return w, nil
}

type s3Writer struct {
	s          *S3
	ctx        context.Context
	key        string
	size, part int64
	upload     string // the multipart upload ID; "" for a single PUT

	buf   []byte // a single PUT's body
	sent  int64  // bytes handed to parts so far
	left  int64  // bytes the open part still takes
	pw    *io.PipeWriter
	done  chan error
	etags []string
}

// open starts the next part, whose body is the next n bytes written.
func (w *s3Writer) open(n int64) {
	pr, pw := io.Pipe()
	w.pw, w.left, w.done = pw, n, make(chan error, 1)
	q := url.Values{"partNumber": {strconv.Itoa(len(w.etags) + 1)}, "uploadId": {w.upload}}
	go func() {
		res, err := w.s.b.Stream(w.ctx, http.MethodPut, w.key, q, pr, n, nil)
		res, err = reply(http.MethodPut, w.key, res, err)
		if err == nil {
			w.etags = append(w.etags, res.Header.Get("ETag"))
			res.Body.Close()
		}
		pr.CloseWithError(errors.Join(err, io.ErrClosedPipe)) // a refused part stops the writes
		w.done <- err
	}()
}

// finish ends the open request and waits for its reply.
func (w *s3Writer) finish() error {
	if w.pw == nil {
		return nil
	}
	w.pw.Close()
	w.pw = nil
	return <-w.done
}

var errGrew = errors.New("the file grew during the copy")

func (w *s3Writer) Write(b []byte) (int, error) {
	if w.upload == "" {
		if int64(len(w.buf)+len(b)) > w.size {
			return 0, errGrew
		}
		w.buf = append(w.buf, b...)
		return len(b), nil
	}
	written := 0
	for len(b) > 0 {
		if w.pw == nil {
			if w.sent >= w.size {
				return written, errGrew
			}
			w.open(min(w.part, w.size-w.sent))
		}
		n, err := w.pw.Write(b[:min(int64(len(b)), w.left)])
		written, w.sent, w.left, b = written+n, w.sent+int64(n), w.left-int64(n), b[n:]
		if err != nil {
			return written, err
		}
		if w.left == 0 {
			if err := w.finish(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (w *s3Writer) Commit() error {
	if w.upload == "" {
		if int64(len(w.buf)) != w.size {
			return fmt.Errorf("the file shrank during the copy: %d bytes of %d", len(w.buf), w.size)
		}
		res, err := w.s.do(w.ctx, http.MethodPut, w.key, nil, w.buf, nil)
		if err == nil {
			res.Body.Close()
		}
		return err
	}
	if w.sent != w.size {
		w.Abort()
		return fmt.Errorf("the file shrank during the copy: %d bytes of %d", w.sent, w.size)
	}
	if err := w.finish(); err != nil {
		w.Abort()
		return err
	}
	var body strings.Builder
	body.WriteString("<CompleteMultipartUpload>")
	for i, e := range w.etags {
		fmt.Fprintf(&body, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, xmlEscape(e))
	}
	body.WriteString("</CompleteMultipartUpload>")
	res, err := w.s.b.Do(w.ctx, http.MethodPost, w.key, url.Values{"uploadId": {w.upload}}, []byte(body.String()), nil)
	if err == nil {
		defer res.Body.Close()
		// Complete can fail after its 200 has been sent, with an error
		// document as the body.
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
		if res.StatusCode != http.StatusOK || s3Message(msg) != "" {
			err = fmt.Errorf("S3 did not complete the upload: %s %s", res.Status, s3Message(msg))
		}
	}
	if err != nil {
		w.Abort()
	}
	return err
}

func (w *s3Writer) Abort() error {
	if w.pw != nil {
		w.pw.CloseWithError(errAborted)
		<-w.done
		w.pw = nil
	}
	if w.upload == "" {
		return nil
	}
	// The task's context is already cancelled when a cancel aborts.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), dialTimeout)
	defer cancel()
	res, err := w.s.do(ctx, http.MethodDelete, w.key, url.Values{"uploadId": {w.upload}}, nil, nil)
	if err == nil {
		res.Body.Close()
	}
	w.upload = ""
	return err
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Mkdir makes a folder marker, so an empty folder shows.
func (s *S3) Mkdir(ctx context.Context, dir, rel string) error {
	res, err := s.do(ctx, http.MethodPut, key(path.Join(dir, rel))+"/", nil, nil, nil)
	if err == nil {
		res.Body.Close()
	}
	return err
}

// Rename copies and then deletes, object by object: S3 has no rename.
//
// shortcut: CopyObject takes objects up to 5 GiB; copy larger ones with
// UploadPartCopy when that limit is met.
func (s *S3) Rename(ctx context.Context, from, to string) error {
	e, err := s.Stat(ctx, from)
	if err != nil {
		return err
	}
	if !e.IsDir {
		return s.move(ctx, key(from), key(to))
	}
	l, err := s.list(ctx, dirPrefix(from), "", 0)
	if err != nil {
		return err
	}
	for _, c := range l.Contents {
		if err := s.move(ctx, c.Key, dirPrefix(to)+strings.TrimPrefix(c.Key, dirPrefix(from))); err != nil {
			return err
		}
	}
	return nil
}

func (s *S3) move(ctx context.Context, from, to string) error {
	h := http.Header{"X-Amz-Copy-Source": {"/" + s3sig.Escape(s.b.Name+"/"+from)}}
	res, err := s.do(ctx, http.MethodPut, to, nil, nil, h)
	if err != nil {
		return err
	}
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	res.Body.Close()
	if m := s3Message(msg); m != "" { // a copy can fail after its 200
		return fmt.Errorf("S3 did not copy %s: %s", from, m)
	}
	return s.delete(ctx, from)
}

func (s *S3) delete(ctx context.Context, k string) error {
	res, err := s.do(ctx, http.MethodDelete, k, nil, nil, nil)
	if err == nil {
		res.Body.Close()
	}
	return err
}

// Remove deletes an object, or an empty folder's marker.
func (s *S3) Remove(ctx context.Context, p string, isDir bool) error {
	if !isDir {
		return s.delete(ctx, key(p))
	}
	l, err := s.list(ctx, dirPrefix(p), "/", 2)
	if err != nil {
		return err
	}
	for _, c := range l.Contents {
		if c.Key != dirPrefix(p) {
			return fmt.Errorf("%s: the folder is not empty", p)
		}
	}
	if len(l.CommonPrefixes) > 0 {
		return fmt.Errorf("%s: the folder is not empty", p)
	}
	return s.delete(ctx, dirPrefix(p))
}
