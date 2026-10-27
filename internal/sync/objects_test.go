package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	gosync "sync"
	"testing"
)

// objectServer is an in-process WebDAV/S3 stand-in with real conditional
// PUT semantics, or none when lax (a target that ignores them).
type objectServer struct {
	mu      gosync.Mutex
	objects map[string][]byte
	lax     bool
	mkcol   bool // PUT answers 409 until MKCOL, as WebDAV does for a missing folder
	authz   []string
}

func newObjectServer(t *testing.T, lax, needMkcol bool) (*objectServer, *httptest.Server) {
	o := &objectServer{objects: map[string][]byte{}, lax: lax, mkcol: !needMkcol}
	srv := httptest.NewServer(o)
	t.Cleanup(srv.Close)
	return o, srv
}

func etag(b []byte) string {
	s := sha256.Sum256(b)
	return `"` + hex.EncodeToString(s[:8]) + `"`
}

func (o *objectServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.authz = append(o.authz, r.Header.Get("Authorization"))
	cur, exists := o.objects[r.URL.Path]
	switch r.Method {
	case "MKCOL":
		o.mkcol = true
		w.WriteHeader(http.StatusCreated)
	case http.MethodGet, http.MethodHead:
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", etag(cur))
		w.Write(cur)
	case http.MethodDelete:
		delete(o.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPut:
		if !o.mkcol {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if !o.lax {
			if im := r.Header.Get("If-Match"); im != "" && (!exists || im != etag(cur)) {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			if r.Header.Get("If-None-Match") == "*" && exists {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
		b, _ := io.ReadAll(r.Body)
		o.objects[r.URL.Path] = b
		w.WriteHeader(http.StatusCreated)
	}
}

func casContract(t *testing.T, a, b Backend) {
	t.Helper()
	ctx := context.Background()
	if err := ProbeCAS(ctx, a); err != nil {
		t.Fatalf("probe: %v", err)
	}
	files, v0, err := a.Fetch(ctx)
	if err != nil || len(files) != 0 || v0 != "" {
		t.Fatalf("empty Fetch = %v, %q, %v", files, v0, err)
	}
	if err := a.Store(ctx, SharedName, []byte("one"), ""); err != nil {
		t.Fatal(err)
	}
	if err := b.Store(ctx, SharedName, []byte("racing"), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("second create = %v, want ErrConflict", err)
	}
	files, v1, err := b.Fetch(ctx)
	if err != nil || len(files) != 1 || string(files[0].Data) != "one" {
		t.Fatalf("Fetch = %v, %v", files, err)
	}
	if err := a.Store(ctx, SharedName, []byte("two"), v1); err != nil {
		t.Fatal(err)
	}
	if err := b.Store(ctx, SharedName, []byte("stale"), v1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale replace = %v, want ErrConflict", err)
	}
	if v, err := b.Version(ctx); err != nil || v == v1 {
		t.Fatalf("Version = %q, %v; want a change after the upload", v, err)
	}
}

func TestWebDAVReplacesOnlyTheVersionItSaw(t *testing.T) {
	_, srv := newObjectServer(t, false, true)
	target := Target{Kind: "webdav", URL: srv.URL + "/dav/hostbay", User: "u"}
	a, err := Connect(context.Background(), target, "pw", Deps{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Connect(context.Background(), target, "pw", Deps{})
	casContract(t, a, b)
}

func TestS3ReplacesOnlyTheVersionItSawAndSigns(t *testing.T) {
	o, srv := newObjectServer(t, false, false)
	target := Target{Kind: "s3", URL: srv.URL, Bucket: "bkt", Path: "hb", User: "AKIDEXAMPLE", Region: "eu-west-1"}
	a, err := Connect(context.Background(), target, "secret", Deps{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Connect(context.Background(), target, "secret", Deps{})
	casContract(t, a, b)
	if _, ok := o.objects["/bkt/hb/"+SharedName]; !ok {
		t.Fatalf("objects at %v, want /bkt/hb/%s", o.objects, SharedName)
	}
	for _, a := range o.authz {
		if !strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || strings.Contains(a, "secret") {
			t.Fatalf("Authorization = %q", a)
		}
	}
}

func TestTargetsThatIgnoreConditionalUploadsAreRefused(t *testing.T) {
	_, srv := newObjectServer(t, true, false)
	for _, target := range []Target{
		{Kind: "s3", URL: srv.URL, Bucket: "bkt", User: "AK"},
		{Kind: "webdav", URL: srv.URL + "/dav"},
	} {
		b, err := Connect(context.Background(), target, "secret", Deps{})
		if err != nil {
			t.Fatal(err)
		}
		if err := ProbeCAS(context.Background(), b); err == nil {
			t.Fatalf("%s target that ignores If-None-Match passed the probe", target.Kind)
		}
	}
}
