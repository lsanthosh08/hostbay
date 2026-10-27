package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Repo is this project's GitHub repository, the only place updates come from.
const Repo = "lsanthosh08/hostbay"

// githubAPI is fixed: no mirror and no environment override (KTD21).
const githubAPI = "https://api.github.com"

// maxManifest caps the manifest, signature and releases list downloads.
const maxManifest = 1 << 20

// metadataTimeout caps one fetch of the releases list, manifest or
// signature; the client's own timeout is sized for a whole installer.
// Tests shorten it.
var metadataTimeout = 30 * time.Second

// ErrNotSigned is returned when this build has no release key.
var ErrNotSigned = errors.New("updates are not signed yet")

// Signed reports whether this build carries a release key, and so can
// update itself at all.
func Signed() bool { return len(releaseKeys) > 0 }

// Updater checks for and downloads verified updates.
type Updater struct {
	current      string
	keys         []ed25519.PublicKey
	goos, goarch string
	appImage     bool // running as an AppImage, the one Linux format it can replace
	// api is the GitHub API base URL. Only tests point it elsewhere.
	api    string
	client *http.Client
}

// New returns an Updater for the running build at version current.
func New(current string) *Updater {
	keys, err := parseKeys(releaseKeys)
	if err != nil {
		// A malformed built-in key is a build mistake that TestEmbeddedKeysParse
		// catches; refuse every update rather than trust part of the list.
		keys = nil
	}
	return &Updater{
		current: current, keys: keys,
		goos: runtime.GOOS, goarch: runtime.GOARCH,
		appImage: os.Getenv("APPIMAGE") != "",
		api:      githubAPI,
		client:   newClient(http.DefaultTransport),
	}
}

// newClient follows redirects only to HTTPS. Asset bytes may come from any
// host GitHub redirects to, because the signed size and SHA-256 decide what
// installs.
func newClient(rt http.RoundTripper) *http.Client {
	return &http.Client{
		Transport: rt,
		Timeout:   30 * time.Minute, // a whole installer on a slow link
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing a redirect to %s", req.URL.Redacted())
			}
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// Release is a verified newer release.
type Release struct {
	Version string `json:"version"`
	PageURL string `json:"pageUrl"` // the GitHub release page
	// Asset is the file this install updates from, or nil when it cannot
	// update itself (deb and rpm) and the UI links PageURL instead.
	Asset    *Asset `json:"asset"`
	assetURL string
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Check returns the newest release if it verifies and is newer than the
// running version, or nil when there is none. prerelease includes GitHub
// pre-releases. Nothing but the releases list, manifest and signature is
// fetched.
func (u *Updater) Check(ctx context.Context, prerelease bool) (*Release, error) {
	if len(u.keys) == 0 {
		return nil, ErrNotSigned
	}
	b, err := u.get(ctx, u.api+"/repos/"+Repo+"/releases?per_page=20", maxManifest)
	if err != nil {
		return nil, err
	}
	var list []ghRelease
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("read the releases list: %w", err)
	}
	var rel *ghRelease
	for i := range list {
		// A "-" tag (v1.2.0-rc1) is a pre-release even when it was not
		// published as one.
		pre := list[i].Prerelease || strings.Contains(list[i].TagName, "-")
		if !list[i].Draft && (prerelease || !pre) {
			rel = &list[i]
			break
		}
	}
	if rel == nil {
		return nil, nil
	}
	urls := map[string]string{}
	for _, a := range rel.Assets {
		urls[a.Name] = a.URL
	}
	if urls[ManifestName] == "" || urls[SignatureName] == "" {
		return nil, fmt.Errorf("the latest release is not signed: %s", rel.HTMLURL)
	}
	manifest, err := u.get(ctx, urls[ManifestName], maxManifest)
	if err != nil {
		return nil, err
	}
	sig, err := u.get(ctx, urls[SignatureName], maxManifest)
	if err != nil {
		return nil, err
	}
	m, a, err := u.verify(manifest, sig, prerelease)
	if err != nil {
		if errors.Is(err, errNotNewer) {
			return nil, nil
		}
		return nil, err
	}
	r := &Release{Version: m.Version, PageURL: rel.HTMLURL, Asset: a}
	if a != nil {
		if r.assetURL = urls[a.Name]; r.assetURL == "" {
			return nil, fmt.Errorf("the release has no file %s", a.Name)
		}
	}
	return r, nil
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	resp, err := u.open(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", url, limit)
	}
	return b, nil
}

func (u *Updater) open(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(url, u.api) {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// Download fetches r's asset into dir and returns its path once its size and
// SHA-256 match the signed manifest. A file that does not match is deleted.
func (u *Updater) Download(ctx context.Context, r *Release, dir string) (string, error) {
	if r == nil || r.Asset == nil {
		return "", errors.New("this install updates from the release page")
	}
	a := r.Asset
	resp, err := u.open(ctx, r.assetURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.ContentLength > a.Size {
		return "", fmt.Errorf("%s is larger than the signed %d bytes", a.Name, a.Size)
	}
	path := filepath.Join(dir, filepath.Base(a.Name))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, a.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = matches(n, h.Sum(nil), *a)
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// matches compares a file's length and SHA-256 with the signed asset.
func matches(n int64, sum []byte, a Asset) error {
	if n != a.Size {
		return fmt.Errorf("%s is %d bytes, the signed size is %d", a.Name, n, a.Size)
	}
	if hex.EncodeToString(sum) != a.SHA256 {
		return fmt.Errorf("%s does not match its signed SHA-256", a.Name)
	}
	return nil
}
