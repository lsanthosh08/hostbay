package secrets

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zalando/go-keyring"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
)

func TestMasterKeyIsReusedAcrossOpens(t *testing.T) {
	v, st := newTestVault(t, nil)
	sealed, err := v.Seal([]byte("pw"), webAAD)
	if err != nil {
		t.Fatal(err)
	}
	got, err := New(st, nil).Open(sealed, webAAD)
	if err != nil || string(got) != "pw" {
		t.Fatalf("Open from a new vault = %q, %v; want pw", got, err)
	}
}

func TestSealWithDeniedKeychainNeedsSecret(t *testing.T) {
	v, _ := newTestVault(t, nil)
	keyring.MockInitWithError(errors.New("user denied keychain access"))
	sealed, err := v.Seal([]byte("pw"), webAAD)
	if apperr.CodeOf(err) != apperr.SecretNeeded || sealed != "" {
		t.Fatalf("Seal = %q, %v; want empty, SecretNeeded", sealed, err)
	}
}

// snapshot reads every file in dir, so a test can prove nothing was rewritten.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range entries {
		if e.Type().IsRegular() {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			files[e.Name()] = string(b)
		}
	}
	return files
}

func TestMasterPasswordModeUnlocksOnlyWithTheRightPassword(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Hostbay")
	keyring.MockInitWithError(errors.New("no Secret Service"))
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v := New(st, nil)
	v.requirePassword = true
	if err := v.SetMasterPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal([]byte("pw"), webAAD)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(st, "hosts.json", map[string]string{"password": sealed}); err != nil {
		t.Fatal(err)
	}

	locked := New(st, nil)
	locked.requirePassword = true
	if _, err := locked.Open(sealed, webAAD); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("Open while locked: %v; want SecretNeeded", err)
	}
	before := snapshot(t, dir)
	if err := locked.Unlock("wrong horse"); apperr.CodeOf(err) != apperr.VerifyFailed {
		t.Fatalf("Unlock(wrong) = %v; want VerifyFailed", err)
	}
	if after := snapshot(t, dir); !maps.Equal(before, after) {
		t.Fatalf("wrong password changed files:\nbefore %v\nafter  %v", before, after)
	}
	if _, err := locked.Open(sealed, webAAD); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("Open after wrong password: %v; want SecretNeeded", err)
	}
	if err := locked.Unlock("correct horse"); err != nil {
		t.Fatal(err)
	}
	if got, err := locked.Open(sealed, webAAD); err != nil || string(got) != "pw" {
		t.Fatalf("Open after unlock = %q, %v; want pw", got, err)
	}
}

func TestSealNeedsMasterPasswordWhenRequired(t *testing.T) {
	v, st := newTestVault(t, nil)
	v.requirePassword = true
	sealed, err := v.Seal([]byte("pw"), webAAD)
	if apperr.CodeOf(err) != apperr.VerifyFailed || sealed != "" {
		t.Fatalf("Seal before a master password = %q, %v; want empty, VerifyFailed", sealed, err)
	}
	if err := v.SetMasterPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Seal([]byte("pw"), webAAD); err != nil {
		t.Fatalf("Seal after setting a master password: %v", err)
	}
	before, _ := store.Load[masterPassword](st, masterPasswordFile)
	if err := v.SetMasterPassword("another one"); err == nil {
		t.Fatal("SetMasterPassword replaced an existing master password")
	}
	if after, _ := store.Load[masterPassword](st, masterPasswordFile); after.Verifier != before.Verifier {
		t.Fatal("refused SetMasterPassword still rewrote the record")
	}
}

func TestFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permissions")
	}
	keyring.MockInit()
	dir := filepath.Join(t.TempDir(), "Hostbay")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := New(st, nil).SetMasterPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, masterPasswordFile): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", path, got, want)
		}
	}
}
