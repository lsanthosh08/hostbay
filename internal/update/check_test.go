package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub serves one releases list and its asset files, and counts every
// asset request.
type fakeGitHub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
	// prerelease marks the listed release as a GitHub pre-release.
	prerelease bool
	// tag is the listed release's tag; empty means "v9".
	tag string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{files: map[string][]byte{}, hits: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+Repo+"/releases", func(w http.ResponseWriter, r *http.Request) {
		type asset struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}
		var assets []asset
		f.mu.Lock()
		for name := range f.files {
			assets = append(assets, asset{name, f.srv.URL + "/redirect/" + name})
		}
		tag := f.tag
		f.mu.Unlock()
		if tag == "" {
			tag = "v9"
		}
		json.NewEncoder(w).Encode([]any{map[string]any{
			"tag_name": tag, "html_url": "https://github.com/" + Repo + "/releases/tag/v9",
			"draft": false, "prerelease": f.prerelease, "assets": assets,
		}})
	})
	// GitHub answers asset URLs with a redirect to its storage host.
	mux.HandleFunc("/redirect/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dl/"+strings.TrimPrefix(r.URL.Path, "/redirect/"), http.StatusFound)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		f.mu.Lock()
		f.hits[name]++
		b, ok := f.files[name]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) hitCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[name]
}

// publish lists a release whose manifest names one darwin asset with body,
// signed by priv. It returns the asset's name.
func (f *fakeGitHub) publish(t *testing.T, priv ed25519.PrivateKey, version string, body []byte) string {
	sum := sha256.Sum256(body)
	a := Asset{Name: "Hostbay-" + version + "-macos-universal.zip", OS: "darwin", Arch: "universal", Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])}
	m, sig := sign(t, priv, Manifest{Version: version, Assets: []Asset{a}})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[ManifestName] = m
	f.files[SignatureName] = sig
	f.files[a.Name] = body
	return a.Name
}

func (f *fakeGitHub) updater(keys ...ed25519.PublicKey) *Updater {
	u := testUpdater("1.0.0", "darwin", "arm64", keys...)
	u.api = f.srv.URL
	u.client = newClient(f.srv.Client().Transport)
	return u
}

func TestCheckRefusesABadSignatureAndDownloadsNothing(t *testing.T) {
	f := newFakeGitHub(t)
	pub, _ := newKey(t)
	_, other := newKey(t)
	name := f.publish(t, other, "1.1.0", []byte("app"))
	r, err := f.updater(pub).Check(context.Background(), false)
	if err == nil {
		t.Fatalf("Check = %+v, want a refusal", r)
	}
	if n := f.hitCount(name); n != 0 {
		t.Fatalf("the asset was fetched %d times", n)
	}
}

func TestCheckWithoutKeysNeverContactsGitHub(t *testing.T) {
	f := newFakeGitHub(t)
	_, priv := newKey(t)
	f.publish(t, priv, "1.1.0", []byte("app"))
	if _, err := f.updater().Check(context.Background(), false); !errors.Is(err, ErrNotSigned) {
		t.Fatalf("Check = %v, want ErrNotSigned", err)
	}
	if n := f.hitCount(ManifestName); n != 0 {
		t.Fatalf("the manifest was fetched %d times", n)
	}
	if _, err := New("1.0.0").Check(context.Background(), false); len(releaseKeys) == 0 && !errors.Is(err, ErrNotSigned) {
		t.Fatalf("this build has no keys, Check = %v", err)
	}
}

func TestCheckAndDownloadAVerifiedRelease(t *testing.T) {
	f := newFakeGitHub(t)
	pub, priv := newKey(t)
	name := f.publish(t, priv, "1.1.0", []byte("new app"))
	u := f.updater(pub)
	r, err := u.Check(context.Background(), false)
	if err != nil || r == nil || r.Version != "1.1.0" || r.Asset == nil || r.Asset.Name != name {
		t.Fatalf("Check = %+v, %v", r, err)
	}
	if r.PageURL != "https://github.com/"+Repo+"/releases/tag/v9" {
		t.Fatalf("PageURL = %q", r.PageURL)
	}
	path, err := u.Download(context.Background(), r, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new app" {
		t.Fatalf("downloaded %q", b)
	}
}

func TestCheckSkipsAPreReleaseUnlessChosen(t *testing.T) {
	f := newFakeGitHub(t)
	f.prerelease = true
	pub, priv := newKey(t)
	f.publish(t, priv, "1.1.0-rc1", []byte("app"))
	if r, err := f.updater(pub).Check(context.Background(), false); r != nil || err != nil {
		t.Fatalf("Check = %+v, %v; want no update", r, err)
	}
	if r, err := f.updater(pub).Check(context.Background(), true); err != nil || r == nil {
		t.Fatalf("chosen pre-release: Check = %+v, %v", r, err)
	}
}

// A "-" tag published without the pre-release flag still reaches only
// users who chose pre-releases.
func TestCheckSkipsAnUnflaggedRCTag(t *testing.T) {
	f := newFakeGitHub(t)
	f.tag = "v1.1.0-rc1"
	pub, priv := newKey(t)
	f.publish(t, priv, "1.1.0-rc1", []byte("app"))
	if r, err := f.updater(pub).Check(context.Background(), false); r != nil || err != nil {
		t.Fatalf("Check = %+v, %v; want no update", r, err)
	}
	if r, err := f.updater(pub).Check(context.Background(), true); err != nil || r == nil {
		t.Fatalf("chosen pre-release: Check = %+v, %v", r, err)
	}
}

func TestDownloadWithAWrongHashIsDeleted(t *testing.T) {
	f := newFakeGitHub(t)
	pub, priv := newKey(t)
	name := f.publish(t, priv, "1.1.0", []byte("new app"))
	u := f.updater(pub)
	r, err := u.Check(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.files[name] = []byte("bad app") // same size, other bytes
	f.mu.Unlock()
	dir := t.TempDir()
	if path, err := u.Download(context.Background(), r, dir); err == nil {
		t.Fatalf("Download = %s, want a hash mismatch", path)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
}

func TestDownloadStopsAtTheSignedSize(t *testing.T) {
	f := newFakeGitHub(t)
	pub, priv := newKey(t)
	name := f.publish(t, priv, "1.1.0", []byte("new app"))
	u := f.updater(pub)
	r, err := u.Check(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.files[name] = []byte("new app and then much more")
	f.mu.Unlock()
	dir := t.TempDir()
	if _, err := u.Download(context.Background(), r, dir); err == nil {
		t.Fatal("an oversized download was accepted")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
}

func TestClientRefusesARedirectToPlainHTTP(t *testing.T) {
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	tls := httptest.NewTLSServer(http.RedirectHandler(plain.URL+"/x", http.StatusFound))
	defer tls.Close()
	resp, err := newClient(tls.Client().Transport).Get(tls.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("followed a redirect to http")
	}
}

func TestCheckGivesUpOnAReleasesListThatNeverArrives(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	old := metadataTimeout
	metadataTimeout = 100 * time.Millisecond
	t.Cleanup(func() { metadataTimeout = old })

	pub, _ := newKey(t)
	u := testUpdater("1.0.0", "darwin", "arm64", pub)
	u.api = srv.URL
	u.client = newClient(srv.Client().Transport)
	done := make(chan error, 1)
	go func() {
		_, err := u.Check(context.Background(), false)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Check = %v, want a timeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Check still waiting on the releases list")
	}
}
