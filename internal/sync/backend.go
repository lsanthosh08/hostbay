package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"hostbay/internal/sshx"
)

// File is one snapshot file on a target.
type File struct {
	Name string
	Data []byte
}

// Backend is a sync target. Git, WebDAV and S3 hold one shared snapshot and
// replace it by compare-and-swap. A folder, SFTP and SMB cannot hold a
// reliable lock (a Dropbox or OneDrive folder replicates a lock file too
// late), so there each computer writes its own files, named by device and
// generation, and never overwrites one (KTD13).
type Backend interface {
	// Fetch returns the snapshot files to merge, at most one per device,
	// and the target's version for Store.
	Fetch(ctx context.Context) ([]File, string, error)
	// Store uploads data as name. A shared target is replaced only while it
	// is still at version ("" when it had no snapshot); a per-device file is
	// only ever created. Either returns ErrConflict when that does not hold.
	Store(ctx context.Context, name string, data []byte, version string) error
	// Version is the cheap change check: it differs whenever Fetch would.
	Version(ctx context.Context) (string, error)
	// Prune removes per-device files a newer one from the same device
	// supersedes. Shared targets have nothing to prune.
	Prune(ctx context.Context) error
	PerDevice() bool
	Close() error
}

// SharedName is the snapshot file on a shared target.
const SharedName = "hostbay.sync"

const deviceFilePrefix, deviceFileSuffix = "hostbay-", ".sync"

// deviceFileName names a per-device file so names sort by generation.
func deviceFileName(device string, gen uint64) string {
	return fmt.Sprintf("%s%s-%020d%s", deviceFilePrefix, device, gen, deviceFileSuffix)
}

func parseDeviceFileName(name string) (device string, gen uint64, ok bool) {
	body, ok := strings.CutPrefix(name, deviceFilePrefix)
	if !ok {
		return "", 0, false
	}
	if body, ok = strings.CutSuffix(body, deviceFileSuffix); !ok {
		return "", 0, false
	}
	i := strings.LastIndexByte(body, '-')
	if i <= 0 {
		return "", 0, false
	}
	gen, err := strconv.ParseUint(body[i+1:], 10, 64)
	return body[:i], gen, err == nil
}

// fileStore is the little a per-device target must do.
type fileStore interface {
	List(ctx context.Context) ([]string, error)
	Read(ctx context.Context, name string) ([]byte, error)
	// Create writes name whole, so a reader never sees part of it, and
	// returns ErrConflict if name exists.
	Create(ctx context.Context, name string, data []byte) error
	Remove(ctx context.Context, name string) error
	Close() error
}

// deviceFiles turns a fileStore into a per-device Backend.
type deviceFiles struct{ fs fileStore }

// latest maps each device to its newest file, and lists the rest.
func (b deviceFiles) latest(ctx context.Context) (map[string]string, []string, error) {
	names, err := b.fs.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	slices.Sort(names)
	newest := map[string]string{}
	var older []string
	for _, n := range names {
		dev, _, ok := parseDeviceFileName(n)
		if !ok {
			continue
		}
		if prev, ok := newest[dev]; ok {
			older = append(older, prev)
		}
		newest[dev] = n
	}
	return newest, older, nil
}

func (b deviceFiles) Fetch(ctx context.Context) ([]File, string, error) {
	newest, _, err := b.latest(ctx)
	if err != nil {
		return nil, "", err
	}
	var files []File
	for _, n := range newest {
		data, err := b.fs.Read(ctx, n)
		if err != nil {
			return nil, "", fmt.Errorf("read %s: %w", n, err)
		}
		files = append(files, File{Name: n, Data: data})
	}
	slices.SortFunc(files, func(a, c File) int { return strings.Compare(a.Name, c.Name) })
	return files, versionOf(newest), nil
}

func (b deviceFiles) Version(ctx context.Context) (string, error) {
	newest, _, err := b.latest(ctx)
	return versionOf(newest), err
}

func versionOf(newest map[string]string) string {
	names := make([]string, 0, len(newest))
	for _, n := range newest {
		names = append(names, n)
	}
	slices.Sort(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return hex.EncodeToString(sum[:])
}

func (b deviceFiles) Store(ctx context.Context, name string, data []byte, _ string) error {
	return b.fs.Create(ctx, name, data)
}

func (b deviceFiles) Prune(ctx context.Context) error {
	_, older, err := b.latest(ctx)
	if err != nil {
		return err
	}
	for _, n := range older {
		if err := b.fs.Remove(ctx, n); err != nil {
			return fmt.Errorf("remove %s: %w", n, err)
		}
	}
	return nil
}

func (b deviceFiles) PerDevice() bool { return true }
func (b deviceFiles) Close() error    { return b.fs.Close() }

// Target says where sync goes. Which fields matter depends on Kind:
//
//	git:    URL (https:// or a local path), Branch, User
//	webdav: URL of the folder, User
//	folder: Path
//	s3:     URL (endpoint), Region, Bucket, Path (key prefix), User (access key)
//	sftp:   Host, Port, User, Path
//	smb:    Host, Port, User, Domain, Share, Path
//
// The secret (token, password or S3 secret key) is never part of it.
type Target struct {
	Kind   string `json:"kind"`
	URL    string `json:"url,omitempty"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	User   string `json:"user,omitempty"`
	Path   string `json:"path,omitempty"`
	Branch string `json:"branch,omitempty"`
	Region string `json:"region,omitempty"`
	Bucket string `json:"bucket,omitempty"`
	Share  string `json:"share,omitempty"`
	Domain string `json:"domain,omitempty"`
}

// Where is the target as one string, for binding its sealed secrets.
func (t Target) Where() string {
	return strings.Join([]string{t.Kind, t.URL, t.Host, strconv.Itoa(t.Port), t.User, t.Path, t.Branch, t.Region, t.Bucket, t.Share, t.Domain}, "\x00")
}

// Deps are what some backends need from the app.
type Deps struct {
	// Dialer verifies SFTP host keys against known_hosts (KTD4).
	Dialer *sshx.Dialer
	// CacheDir keeps the Git backend's local clone between syncs.
	CacheDir string
	// HTTP is used by WebDAV and S3; nil means http.DefaultClient.
	HTTP *http.Client
}

// Connect opens the target t with secret.
func Connect(ctx context.Context, t Target, secret string, deps Deps) (Backend, error) {
	switch t.Kind {
	case "folder":
		if t.Path == "" {
			return nil, errors.New("choose a folder for sync")
		}
		return NewFolder(t.Path), nil
	case "git":
		return NewGit(t, secret, deps.CacheDir)
	case "webdav":
		return NewWebDAV(t, secret, deps.HTTP)
	case "s3":
		return NewS3(t, secret, deps.HTTP)
	case "sftp":
		return NewSFTP(ctx, t, secret, deps.Dialer)
	case "smb":
		return NewSMB(ctx, t, secret)
	}
	return nil, fmt.Errorf("unknown sync target %q", t.Kind)
}

// ProbeCAS checks that a shared target refuses an upload made against a
// stale version. A target that accepts both would let two computers
// overwrite each other, so sync must not be turned on there (KTD13). Git
// needs no probe: a push that is not a fast-forward is always refused.
func ProbeCAS(ctx context.Context, b Backend) error {
	if p, ok := b.(interface{ probe(context.Context) error }); ok {
		return p.probe(ctx)
	}
	return nil
}
