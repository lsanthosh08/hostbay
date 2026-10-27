package update

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenVerifiedLocksTheFileAgainstChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setup.exe")
	os.WriteFile(path, []byte("installer"), 0o600)
	sum := sha256.Sum256([]byte("installer"))
	a := Asset{Name: "setup.exe", Size: 9, SHA256: hex.EncodeToString(sum[:])}

	f, err := openVerified(path, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("swapped!!"), 0o600); err == nil {
		t.Fatal("the installer was rewritten while held")
	}
	if err := os.Rename(path, path+".old"); err == nil {
		t.Fatal("the installer was moved while held")
	}
	f.Close()

	os.WriteFile(path, []byte("swapped!!"), 0o600)
	if f, err := openVerified(path, a); err == nil {
		f.Close()
		t.Fatal("a changed installer verified")
	}
}
