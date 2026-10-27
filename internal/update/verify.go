package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// errNotNewer marks a signed release that is not newer than this build.
var errNotNewer = errors.New("not newer than")

func parseKeys(b64 []string) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(b64))
	for _, s := range b64 {
		k, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("release key %q is not a base64 ed25519 public key", s)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// verify checks the manifest's signature against the built-in keys before
// reading anything in it, then refuses a version not newer than the running
// one, a pre-release unless chosen, and a release with no build for this
// OS and architecture. The returned asset is nil when this install cannot
// update itself (deb and rpm).
func (u *Updater) verify(manifest, sig []byte, prerelease bool) (Manifest, *Asset, error) {
	s, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(sig)))
	if err != nil {
		return Manifest{}, nil, errors.New("the update signature is malformed")
	}
	signed := false
	for _, k := range u.keys {
		if ed25519.Verify(k, manifest, s) {
			signed = true
			break
		}
	}
	if !signed {
		return Manifest{}, nil, errors.New("the update is not signed by a Hostbay release key")
	}
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return Manifest{}, nil, fmt.Errorf("read the update manifest: %w", err)
	}
	newer, err := newerThan(m.Version, u.current)
	if err != nil {
		return Manifest{}, nil, err
	}
	if !newer {
		return Manifest{}, nil, fmt.Errorf("update %s: %w %s", m.Version, errNotNewer, u.current)
	}
	if strings.Contains(m.Version, "-") && !prerelease {
		return Manifest{}, nil, fmt.Errorf("update %s is a pre-release", m.Version)
	}
	suffix := u.installSuffix()
	found := false
	for i, a := range m.Assets {
		if a.OS != u.goos || (a.Arch != u.goarch && a.Arch != "universal") {
			continue
		}
		found = true
		if suffix != "" && strings.HasSuffix(a.Name, suffix) {
			return m, &m.Assets[i], nil
		}
	}
	if !found {
		return Manifest{}, nil, fmt.Errorf("update %s has no build for %s/%s", m.Version, u.goos, u.goarch)
	}
	return m, nil, nil
}

// installSuffix is the file type this install updates from; "" means it
// cannot update itself.
func (u *Updater) installSuffix() string {
	switch {
	case u.goos == "darwin":
		return ".zip"
	case u.goos == "windows":
		return ".exe"
	case u.goos == "linux" && u.appImage:
		return ".AppImage"
	}
	return ""
}

// newerThan reports whether version a sorts after b. Both are
// MAJOR.MINOR.PATCH with an optional -pre-release suffix, which sorts before
// its final release.
func newerThan(a, b string) (bool, error) {
	ac, ap, err := parseVersion(a)
	if err != nil {
		return false, err
	}
	bc, bp, err := parseVersion(b)
	if err != nil {
		return false, err
	}
	for i := range ac {
		if ac[i] != bc[i] {
			return ac[i] > bc[i], nil
		}
	}
	switch {
	case ap == bp:
		return false, nil
	case ap == "":
		return true, nil
	case bp == "":
		return false, nil
	}
	// shortcut: pre-release suffixes compare as strings, so rc10 sorts before
	// rc9; upgrade to semver's dotted-identifier rules if tags ever need it.
	return ap > bp, nil
}

func parseVersion(v string) ([3]int, string, error) {
	var core [3]int
	num, pre, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	parts := strings.Split(num, ".")
	if len(parts) != 3 {
		return core, "", fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return core, "", fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", v)
		}
		core[i] = n
	}
	return core, pre, nil
}
