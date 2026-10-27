// Command sign-manifest writes and signs the update manifest for a release
// (KTD21). The release workflow runs it in the protected release-signing
// environment over the files it is about to publish:
//
//	HOSTBAY_SIGNING_KEY=... sign-manifest -version 1.2.0 -dir release
//
// It writes hostbay-manifest.json and hostbay-manifest.json.sig into dir.
// The key is the base64 ed25519 seed and is read only from the environment,
// never printed. -keygen PATH makes a new key; see docs/release-signing.md.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"hostbay/internal/update"
)

const keyEnv = "HOSTBAY_SIGNING_KEY"

func main() {
	if err := run(os.Args[1:], os.Getenv(keyEnv), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "sign-manifest:", err)
		os.Exit(1)
	}
}

func run(args []string, key string, out io.Writer) error {
	fs := flag.NewFlagSet("sign-manifest", flag.ContinueOnError)
	version := fs.String("version", "", "release version, without the leading v")
	dir := fs.String("dir", "", "directory holding the release assets")
	keygen := fs.String("keygen", "", "write a new private key to this file and print its public key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *keygen != "" {
		return generate(*keygen, out)
	}
	if *version == "" || *dir == "" {
		return errors.New("-version and -dir are required")
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	if err != nil || len(seed) != ed25519.SeedSize {
		// Never echo the value: it may be most of a real key.
		return fmt.Errorf("%s must hold a base64 ed25519 seed", keyEnv)
	}
	m, err := manifest(*version, *dir)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), b)
	if err := os.WriteFile(filepath.Join(*dir, update.ManifestName), b, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, update.SignatureName), []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "signed %d assets of %s\n", len(m.Assets), *version)
	return nil
}

// manifest lists every file in dir. Each must be named
// Hostbay-VERSION-OS-ARCH[-setup].EXT, as the release workflow names them,
// so nothing is published that the manifest does not cover.
func manifest(version, dir string) (update.Manifest, error) {
	m := update.Manifest{Version: version}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return m, err
	}
	for _, e := range entries {
		name := e.Name()
		if name == update.ManifestName || name == update.SignatureName {
			continue
		}
		goos, arch, err := platform(name, version)
		if err != nil {
			return m, err
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return m, err
		}
		h := sha256.New()
		n, err := io.Copy(h, f)
		f.Close()
		if err != nil {
			return m, err
		}
		m.Assets = append(m.Assets, update.Asset{Name: name, OS: goos, Arch: arch, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
	}
	if len(m.Assets) == 0 {
		return m, fmt.Errorf("no release assets in %s", dir)
	}
	return m, nil
}

// platform maps a release file name to Go's OS and architecture names.
func platform(name, version string) (string, string, error) {
	rest, ok := strings.CutPrefix(name, "Hostbay-"+version+"-")
	if !ok {
		return "", "", fmt.Errorf("%s is not a Hostbay %s release asset", name, version)
	}
	rest = strings.TrimSuffix(strings.TrimSuffix(rest, filepath.Ext(rest)), "-setup")
	osName, arch, _ := strings.Cut(rest, "-")
	goos := map[string]string{"macos": "darwin", "windows": "windows", "linux": "linux"}[osName]
	arch = map[string]string{"universal": "universal", "amd64": "amd64", "x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[arch]
	if goos == "" || arch == "" {
		return "", "", fmt.Errorf("%s names no known OS and architecture", name)
	}
	return goos, arch, nil
}

// generate writes a new base64 seed to path, which must not exist yet, and
// prints the public key to embed in internal/update/keys.go.
func generate(path string, out io.Writer) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(f, base64.StdEncoding.EncodeToString(priv.Seed()))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "private key written to %s\npublic key: %s\n", path, base64.StdEncoding.EncodeToString(pub))
	return nil
}
