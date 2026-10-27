package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// sign returns m's JSON and its base64 signature file.
func sign(t *testing.T, priv ed25519.PrivateKey, m Manifest) ([]byte, []byte) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, b)) + "\n")
}

func testUpdater(current, goos, goarch string, keys ...ed25519.PublicKey) *Updater {
	return &Updater{current: current, keys: keys, goos: goos, goarch: goarch}
}

func release(version string) Manifest {
	return Manifest{Version: version, Assets: []Asset{
		{Name: "Hostbay-" + version + "-macos-universal.zip", OS: "darwin", Arch: "universal", Size: 3, SHA256: "aa"},
		{Name: "Hostbay-" + version + "-windows-amd64-setup.exe", OS: "windows", Arch: "amd64", Size: 3, SHA256: "bb"},
		{Name: "Hostbay-" + version + "-linux-amd64.deb", OS: "linux", Arch: "amd64", Size: 3, SHA256: "cc"},
		{Name: "Hostbay-" + version + "-linux-x86_64.AppImage", OS: "linux", Arch: "amd64", Size: 3, SHA256: "dd"},
	}}
}

func TestVerifyRefusesBadSignature(t *testing.T) {
	pub, _ := newKey(t)
	_, other := newKey(t)
	m, sig := sign(t, other, release("1.1.0"))
	if _, _, err := testUpdater("1.0.0", "darwin", "arm64", pub).verify(m, sig, false); err == nil {
		t.Fatal("a manifest signed by an unknown key verified")
	}
	_, priv := newKey(t)
	m, sig = sign(t, priv, release("1.1.0"))
	m[len(m)-2] = ' ' // one changed byte
	if _, _, err := testUpdater("1.0.0", "darwin", "arm64", priv.Public().(ed25519.PublicKey)).verify(m, sig, false); err == nil {
		t.Fatal("a changed manifest verified")
	}
}

func TestVerifyRefusesOlderOrEqualVersion(t *testing.T) {
	pub, priv := newKey(t)
	for _, v := range []string{"1.2.0", "1.1.9", "0.9.0", "1.2.0-rc1"} {
		m, sig := sign(t, priv, release(v))
		if _, _, err := testUpdater("1.2.0", "darwin", "arm64", pub).verify(m, sig, true); err == nil {
			t.Fatalf("version %s verified against running 1.2.0", v)
		}
	}
}

func TestVerifyRefusesPrereleaseUnlessChosen(t *testing.T) {
	pub, priv := newKey(t)
	m, sig := sign(t, priv, release("1.3.0-rc1"))
	u := testUpdater("1.2.0", "darwin", "arm64", pub)
	if _, _, err := u.verify(m, sig, false); err == nil {
		t.Fatal("a pre-release verified without choosing pre-releases")
	}
	if _, a, err := u.verify(m, sig, true); err != nil || a == nil {
		t.Fatalf("chosen pre-release = %v, %v", a, err)
	}
	// A final release sorts after its own pre-releases.
	m, sig = sign(t, priv, release("1.3.0"))
	if _, _, err := testUpdater("1.3.0-rc1", "darwin", "arm64", pub).verify(m, sig, false); err != nil {
		t.Fatalf("1.3.0 over 1.3.0-rc1: %v", err)
	}
}

func TestVerifyRefusesAnotherOSOrArch(t *testing.T) {
	pub, priv := newKey(t)
	m, sig := sign(t, priv, Manifest{Version: "1.1.0", Assets: []Asset{
		{Name: "Hostbay-1.1.0-linux-amd64.deb", OS: "linux", Arch: "amd64", Size: 3, SHA256: "cc"},
	}})
	for _, p := range [][2]string{{"darwin", "arm64"}, {"windows", "amd64"}, {"linux", "arm64"}} {
		if _, _, err := testUpdater("1.0.0", p[0], p[1], pub).verify(m, sig, false); err == nil {
			t.Fatalf("a linux/amd64 release verified for %s/%s", p[0], p[1])
		}
	}
}

func TestVerifyPicksThisPlatformsAsset(t *testing.T) {
	pub, priv := newKey(t)
	m, sig := sign(t, priv, release("1.1.0"))
	for _, c := range []struct {
		goos, goarch string
		appImage     bool
		want         string // "" means link the release page only
	}{
		{"darwin", "arm64", false, "Hostbay-1.1.0-macos-universal.zip"},
		{"darwin", "amd64", false, "Hostbay-1.1.0-macos-universal.zip"},
		{"windows", "amd64", false, "Hostbay-1.1.0-windows-amd64-setup.exe"},
		{"linux", "amd64", true, "Hostbay-1.1.0-linux-x86_64.AppImage"},
		{"linux", "amd64", false, ""},
	} {
		u := testUpdater("1.0.0", c.goos, c.goarch, pub)
		u.appImage = c.appImage
		_, a, err := u.verify(m, sig, false)
		if err != nil {
			t.Fatalf("%s/%s: %v", c.goos, c.goarch, err)
		}
		got := ""
		if a != nil {
			got = a.Name
		}
		if got != c.want {
			t.Fatalf("%s/%s appImage=%v picked %q, want %q", c.goos, c.goarch, c.appImage, got, c.want)
		}
	}
}

func TestVerifyAcceptsTheSecondOfTwoKeys(t *testing.T) {
	oldPub, _ := newKey(t)
	newPub, newPriv := newKey(t)
	m, sig := sign(t, newPriv, release("1.1.0"))
	got, _, err := testUpdater("1.0.0", "darwin", "arm64", oldPub, newPub).verify(m, sig, false)
	if err != nil || got.Version != "1.1.0" {
		t.Fatalf("verify = %+v, %v", got, err)
	}
}

func TestEmbeddedKeysParse(t *testing.T) {
	keys, err := parseKeys(releaseKeys)
	if err != nil || len(keys) != len(releaseKeys) {
		t.Fatalf("parseKeys = %d keys, %v", len(keys), err)
	}
}
