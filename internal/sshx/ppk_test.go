package sshx

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/apperr"
)

// testPassphrase protects the encrypted fixtures, which PuTTYgen 0.85 made.
const testPassphrase = "hostbay-test"

// fixturePub reads the OpenSSH public key PuTTYgen exported for a fixture,
// so expected values never come from the parser under test.
func fixturePub(t *testing.T, name string) ssh.PublicKey {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".pub"))
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(b)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".ppk"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParsePPKMatchesPuTTYgenPublicKey(t *testing.T) {
	for _, tc := range []struct {
		name, passphrase, comment string
	}{
		{"rsa-v3", "", "rsa-v3"},
		{"ed25519-v2", "", "ed-v2"},
		{"ed25519-v3-enc", testPassphrase, "ed-v3-enc"},
		{"rsa-v2-enc", testPassphrase, "rsa-v2-enc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, comment, err := ParsePPK(readFixture(t, tc.name), []byte(tc.passphrase))
			if err != nil {
				t.Fatalf("ParsePPK: %v", err)
			}
			signer, err := ssh.NewSignerFromKey(key)
			if err != nil {
				t.Fatal(err)
			}
			if want := fixturePub(t, tc.name); !bytes.Equal(signer.PublicKey().Marshal(), want.Marshal()) {
				t.Fatalf("public key differs from PuTTYgen's export")
			}
			if comment != tc.comment {
				t.Fatalf("comment = %q, want %q", comment, tc.comment)
			}
		})
	}
}

func TestParsePPKEncryptedNeedsTheRightPassphrase(t *testing.T) {
	for _, name := range []string{"ed25519-v3-enc", "rsa-v2-enc"} {
		data := readFixture(t, name)
		var missing *ssh.PassphraseMissingError
		if _, _, err := ParsePPK(data, nil); !errors.As(err, &missing) {
			t.Errorf("%s without passphrase: err = %v, want PassphraseMissingError", name, err)
		}
		if _, _, err := ParsePPK(data, []byte("wrong")); !errors.Is(err, x509.IncorrectPasswordError) {
			t.Errorf("%s wrong passphrase: err = %v, want IncorrectPasswordError", name, err)
		}
	}
}

func TestParsePPKRejectsTamperedKey(t *testing.T) {
	data := string(readFixture(t, "ed25519-v2"))
	// Swap the comment: the MAC covers it, so the file no longer verifies.
	tampered := strings.Replace(data, "Comment: ed-v2", "Comment: ed-v9", 1)
	if _, _, err := ParsePPK([]byte(tampered), nil); err == nil {
		t.Fatal("tampered PPK parsed without error")
	}
	if _, _, err := ParsePPK([]byte("not a key"), nil); err == nil {
		t.Fatal("garbage parsed without error")
	}
}

func TestPPKToPEMProducesLoadableOpenSSHKey(t *testing.T) {
	out, err := PPKToPEM(readFixture(t, "rsa-v3"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := pem.Decode(out); b == nil || b.Type != "OPENSSH PRIVATE KEY" {
		t.Fatalf("output is not an OpenSSH PEM block:\n%s", out)
	}
	signer, err := ssh.ParsePrivateKey(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), fixturePub(t, "rsa-v3").Marshal()) {
		t.Fatal("converted key has a different public key")
	}
}

// Port of session.rs loads_openssh_key_with_matching_public_blob.
func TestLoadsOpenSSHKeyWithMatchingPublicBlob(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	block, err := ssh.MarshalPrivateKey(priv, "test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	want, _ := ssh.NewPublicKey(priv.Public())

	signer, err := loadSigner(Auth{KeyPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), want.Marshal()) {
		t.Fatal("loaded key's public blob differs")
	}
}

func TestLoadSignerAsksForMissingPassphrase(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	data := pem.EncodeToMemory(block)

	for _, a := range []Auth{{Key: data}, {Key: data, Passphrase: "nope"}, {Key: readFixture(t, "ed25519-v3-enc")}} {
		if _, err := loadSigner(a); apperr.CodeOf(err) != apperr.SecretNeeded {
			t.Errorf("loadSigner(passphrase %q) = %v, want SecretNeeded", a.Passphrase, err)
		}
	}
	if _, err := loadSigner(Auth{Key: data, Passphrase: "s3cret"}); err != nil {
		t.Fatalf("right passphrase: %v", err)
	}
}

func TestRSASignerOffersSHA2Only(t *testing.T) {
	signer, err := loadSigner(Auth{Key: readFixture(t, "rsa-v3")})
	if err != nil {
		t.Fatal(err)
	}
	ms, ok := signer.(ssh.MultiAlgorithmSigner)
	if !ok {
		t.Fatalf("RSA signer %T does not restrict its algorithms", signer)
	}
	if algos := ms.Algorithms(); slices.Contains(algos, ssh.KeyAlgoRSA) || len(algos) == 0 {
		t.Fatalf("RSA signer algorithms = %v; want rsa-sha2-* only", algos)
	}
}
