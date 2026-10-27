package transfer_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/transfer"
	"hostbay/internal/transfer/sourcetest"
)

// objectStore is an in-process S3 stand-in for bucket "bkt", path-style:
// objects, ListObjectsV2 (two keys a page, to exercise continuation),
// CopyObject and multipart uploads.
type objectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	uploads map[string]map[int][]byte // upload ID -> parts
	aborted []string
	nextID  int
}

func newObjectStore(t *testing.T) (*objectStore, *httptest.Server) {
	o := &objectStore{objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	srv := httptest.NewServer(o)
	t.Cleanup(srv.Close)
	return o, srv
}

func (o *objectStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") || r.Header.Get("X-Amz-Content-Sha256") == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != "bkt" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodGet && key == "" && q.Get("list-type") == "2":
		o.list(w, q)
	case r.Method == http.MethodPost && q.Has("uploads"):
		o.nextID++
		id := strconv.Itoa(o.nextID)
		o.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		o.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", `"part`+strconv.Itoa(n)+`"`)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		parts := o.uploads[q.Get("uploadId")]
		var all []byte
		for i := 1; i <= len(parts); i++ {
			all = append(all, parts[i]...)
		}
		o.objects[key] = all
		delete(o.uploads, q.Get("uploadId"))
		io.WriteString(w, "<CompleteMultipartUploadResult/>")
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		delete(o.uploads, q.Get("uploadId"))
		o.aborted = append(o.aborted, key)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
		src, _ := url.PathUnescape(strings.TrimPrefix(r.Header.Get("X-Amz-Copy-Source"), "/bkt/"))
		b, ok := o.objects[src]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		o.objects[key] = bytes.Clone(b)
		io.WriteString(w, "<CopyObjectResult/>")
	case r.Method == http.MethodPut:
		if int64(len(body)) != r.ContentLength {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		o.objects[key] = body
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		b, ok := o.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Write(b)
	case r.Method == http.MethodDelete:
		delete(o.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func (o *objectStore) list(w http.ResponseWriter, q url.Values) {
	prefix, delim := q.Get("prefix"), q.Get("delimiter")
	type content struct {
		Key  string
		Size int
	}
	var keys []string // keys and common prefixes, in order
	seen := map[string]bool{}
	for k := range o.objects {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if i := strings.Index(k[len(prefix):], delim); delim != "" && i >= 0 {
			k = k[:len(prefix)+i+len(delim)]
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	if tok := q.Get("continuation-token"); tok != "" {
		keys = keys[slices.IndexFunc(keys, func(k string) bool { return k > tok }):]
	}
	page := 2
	if m, _ := strconv.Atoi(q.Get("max-keys")); m > 0 && m < page {
		page = m
	}
	out := struct {
		XMLName        xml.Name  `xml:"ListBucketResult"`
		Contents       []content `xml:"Contents"`
		CommonPrefixes []struct{ Prefix string }
		IsTruncated    bool
		Next           string `xml:"NextContinuationToken,omitempty"`
	}{}
	if len(keys) > page {
		out.IsTruncated, out.Next, keys = true, keys[page-1], keys[:page]
	}
	for _, k := range keys {
		if b, ok := o.objects[k]; ok && (delim == "" || !strings.HasSuffix(k[len(prefix):], delim) || k == prefix) {
			out.Contents = append(out.Contents, content{k, len(b)})
		} else {
			out.CommonPrefixes = append(out.CommonPrefixes, struct{ Prefix string }{k})
		}
	}
	xml.NewEncoder(w).Encode(out)
}

func s3Source(t *testing.T, srv *httptest.Server) *transfer.S3 {
	t.Helper()
	src, err := transfer.DialS3(context.Background(), transfer.S3Config{Endpoint: srv.URL, Bucket: "bkt", PathStyle: true, Access: "AK", Secret: "SK"})
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestS3PassesTheSourceSuite(t *testing.T) {
	_, srv := newObjectStore(t)
	sourcetest.Run(t, s3Source(t, srv), "/")
}

func TestS3MultipartUploadCompletesInOrderAndAbortLeavesNoObject(t *testing.T) {
	o, srv := newObjectStore(t)
	src := s3Source(t, srv)
	ctx := context.Background()
	size := 17 << 20 // three parts of 8 MiB, 8 MiB and 1 MiB
	data := bytes.Repeat([]byte("0123456789abcdef"), size/16)

	w, err := src.Create(ctx, "/", "big.bin", int64(size))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyBuffer(w, bytes.NewReader(data), make([]byte, 100_000)); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := o.objects["big.bin"]; !bytes.Equal(got, data) {
		t.Fatalf("stored %d bytes, want the %d written, in order", len(got), size)
	}

	w, err = src.Create(ctx, "/", "cancelled.bin", int64(size))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data[:9<<20]); err != nil { // past the first part
		t.Fatal(err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.objects["cancelled.bin"]; ok || len(o.uploads) != 0 || !slices.Equal(o.aborted, []string{"cancelled.bin"}) {
		t.Fatalf("after abort: object kept %v, uploads %d, aborted %v", ok, len(o.uploads), o.aborted)
	}
}

func TestS3KeyClimbingOutOfItsFolderIsRefusedOnDownload(t *testing.T) {
	o, srv := newObjectStore(t)
	o.objects["a/ok.txt"] = []byte("fine")
	o.objects["a/../../x"] = []byte("escaped")
	parent := tempDir(t)
	dst := parent + "/dst"
	if err := transfer.Local.Mkdir(transfer.Local{}, context.Background(), parent, "dst"); err != nil {
		t.Fatal(err)
	}

	e, ch := newEngine()
	id := e.Start(s3Source(t, srv), []string{"/a"}, transfer.Local{}, dst)
	evs := wait(t, ch, id, nil)
	last := evs[len(evs)-1]
	if last.State != "failed" || len(last.Failed) != 1 || !strings.Contains(last.Failed[0].Path, "..") {
		t.Fatalf("final %+v", last.Event)
	}
	if got := readFile(t, dst+"/a/ok.txt"); got != "fine" {
		t.Fatalf("ok.txt = %q", got)
	}
	if names := dirNames(t, parent); !slices.Equal(names, []string{"dst"}) {
		t.Fatalf("written outside the destination: %v", names)
	}
}
