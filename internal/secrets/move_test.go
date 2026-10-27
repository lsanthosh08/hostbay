package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"hostbay/internal/store"
)

// moveStore moves home's data to a new folder and returns the store opened
// on the launch after the copy, which has cleaned up the old folder.
func moveStore(t *testing.T, home string) *store.Store {
	t.Helper()
	to := filepath.Join(t.TempDir(), "moved")
	if err := os.Mkdir(to, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RequestMove(to); err != nil {
		t.Fatal(err)
	}
	st.Close()
	for range 2 { // the copying launch, then the cleaning one
		if st, err = store.Open(home); err != nil {
			t.Fatal(err)
		}
		if st.Dir() != to {
			t.Fatalf("Dir() = %s, want %s", st.Dir(), to)
		}
		st.Close()
	}
	st, err = store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSecretsOpenAfterTheDataFolderMoves(t *testing.T) {
	keyring.MockInit()
	home := filepath.Join(t.TempDir(), "Hostbay")
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := New(st, nil).Seal([]byte("pw"), webAAD)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	got, err := New(moveStore(t, home), nil).Open(sealed, webAAD)
	if err != nil || string(got) != "pw" {
		t.Fatalf("Open after the move = %q, %v; want pw from the same keychain key", got, err)
	}
}

func TestMasterPasswordFileMovesAndStillUnlocks(t *testing.T) {
	keyring.MockInitWithError(errors.New("no Secret Service"))
	home := filepath.Join(t.TempDir(), "Hostbay")
	st, err := store.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	v := New(st, nil)
	v.requirePassword = true
	if err := v.SetMasterPassword("correct horse"); err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal([]byte("pw"), webAAD)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	moved := moveStore(t, home)
	if _, err := os.Stat(filepath.Join(home, masterPasswordFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("master-password file still in the old folder: %v", err)
	}
	locked := New(moved, nil)
	locked.requirePassword = true
	if err := locked.Unlock("correct horse"); err != nil {
		t.Fatalf("Unlock after the move: %v", err)
	}
	if got, err := locked.Open(sealed, webAAD); err != nil || string(got) != "pw" {
		t.Fatalf("Open after the move = %q, %v", got, err)
	}
}
