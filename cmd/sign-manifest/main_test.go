package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/update"
)

func releaseDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("bytes of "+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestSignsEveryReleaseAsset(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	seed := base64.StdEncoding.EncodeToString(priv.Seed())
	dir := releaseDir(t,
		"Hostbay-1.2.0-rc1-macos-universal.zip",
		"Hostbay-1.2.0-rc1-windows-amd64-setup.exe",
		"Hostbay-1.2.0-rc1-linux-amd64.deb",
		"Hostbay-1.2.0-rc1-linux-x86_64.rpm",
		"Hostbay-1.2.0-rc1-linux-x86_64.AppImage",
	)
	var out strings.Builder
	if err := run([]string{"-version", "1.2.0-rc1", "-dir", dir}, seed, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), seed) {
		t.Fatal("the private key was printed")
	}
	m, _ := os.ReadFile(filepath.Join(dir, update.ManifestName))
	sig, _ := os.ReadFile(filepath.Join(dir, update.SignatureName))
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(pub, m, s) {
		t.Fatalf("the signature does not verify: %v", err)
	}
	var got update.Manifest
	if err := json.Unmarshal(m, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"Hostbay-1.2.0-rc1-macos-universal.zip":     {"darwin", "universal"},
		"Hostbay-1.2.0-rc1-windows-amd64-setup.exe": {"windows", "amd64"},
		"Hostbay-1.2.0-rc1-linux-amd64.deb":         {"linux", "amd64"},
		"Hostbay-1.2.0-rc1-linux-x86_64.rpm":        {"linux", "amd64"},
		"Hostbay-1.2.0-rc1-linux-x86_64.AppImage":   {"linux", "amd64"},
	}
	if got.Version != "1.2.0-rc1" || len(got.Assets) != len(want) {
		t.Fatalf("manifest = %+v", got)
	}
	for _, a := range got.Assets {
		body := "bytes of " + a.Name
		sum := sha256.Sum256([]byte(body))
		if w, ok := want[a.Name]; !ok || a.OS != w[0] || a.Arch != w[1] || a.Size != int64(len(body)) || a.SHA256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("asset %+v", a)
		}
	}
}

func TestRefusesWithoutAKeyOrWithAnUnknownFile(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	seed := base64.StdEncoding.EncodeToString(priv.Seed())
	for _, c := range []struct {
		key   string
		names []string
	}{
		{"", []string{"Hostbay-1.0.0-linux-amd64.deb"}},
		{"not base64!", []string{"Hostbay-1.0.0-linux-amd64.deb"}},
		{seed, []string{"Hostbay-1.0.0-linux-amd64.deb", "notes.txt"}},
		{seed, []string{"Hostbay-0.9.0-linux-amd64.deb"}},
	} {
		dir := releaseDir(t, c.names...)
		if err := run([]string{"-version", "1.0.0", "-dir", dir}, c.key, io.Discard); err == nil {
			t.Fatalf("signed %v with key %q", c.names, c.key)
		}
		if _, err := os.Stat(filepath.Join(dir, update.ManifestName)); !os.IsNotExist(err) {
			t.Fatalf("wrote a manifest for %v", c.names)
		}
	}
}

func TestKeygenWritesASeedAndPrintsThePublicKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostbay-release.key")
	var out strings.Builder
	if err := run([]string{"-keygen", path}, "", &out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("key file = %q, %v", b, err)
	}
	pub := base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	if !strings.Contains(out.String(), pub) || strings.Contains(out.String(), string(b[:20])) {
		t.Fatalf("output = %q", out.String())
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode())
	}
	if err := run([]string{"-keygen", path}, "", io.Discard); err == nil {
		t.Fatal("keygen overwrote an existing key")
	}
}
