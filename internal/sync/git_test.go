package sync

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
)

func bareRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sync.git")
	if _, err := git.PlainInit(dir, true); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestGitPushesOnlyOnTopOfTheCommitItSaw(t *testing.T) {
	repo := bareRepo(t)
	ctx := context.Background()
	a, err := NewGit(Target{URL: repo}, "", filepath.Join(t.TempDir(), "cache-a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewGit(Target{URL: repo}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	files, v0, err := a.Fetch(ctx)
	if err != nil || len(files) != 0 || v0 != "" {
		t.Fatalf("empty Fetch = %v, %q, %v", files, v0, err)
	}
	if err := a.Store(ctx, SharedName, []byte("one"), ""); err != nil {
		t.Fatal(err)
	}
	if err := b.Store(ctx, SharedName, []byte("racing"), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("unrelated push = %v, want ErrConflict", err)
	}
	files, v1, err := b.Fetch(ctx)
	if err != nil || len(files) != 1 || string(files[0].Data) != "one" {
		t.Fatalf("Fetch = %v, %v", files, err)
	}
	if err := b.Store(ctx, SharedName, []byte("two"), v1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Fetch(ctx); err != nil { // the cached clone fetches what is new
		t.Fatal(err)
	}
	if err := a.Store(ctx, SharedName, []byte("stale"), v1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale push = %v, want ErrConflict", err)
	}
	if v, err := a.Version(ctx); err != nil || v == v1 || v == "" {
		t.Fatalf("Version = %q, %v", v, err)
	}
}

func TestGitStoresConverge(t *testing.T) {
	repo := bareRepo(t)
	gitDevice := func() *device {
		b, err := NewGit(Target{URL: repo}, "", filepath.Join(t.TempDir(), "cache"))
		if err != nil {
			t.Fatal(err)
		}
		return newDevice(t, b)
	}
	a, b := gitDevice(), gitDevice()
	ida := a.addHost(t, "from-a", "pw")
	a.sync(t)
	b.sync(t)
	idb := b.addHost(t, "from-b", "")
	b.sync(t)
	a.sync(t)
	for _, d := range []*device{a, b} {
		got := aliases(t, d)
		if got[ida] != "from-a" || got[idb] != "from-b" {
			t.Fatalf("got %v", got)
		}
	}
	if pw, err := b.password(t, ida); pw != "pw" || err != nil {
		t.Fatalf("B's password = %q, %v", pw, err)
	}
}

func TestGitOverSSHIsRefusedRatherThanUnverified(t *testing.T) {
	for _, u := range []string{"ssh://git@example.com/repo.git", "git@example.com:repo.git"} {
		if _, err := NewGit(Target{URL: u}, "", ""); err == nil {
			t.Fatalf("%s accepted without known_hosts checks", u)
		}
	}
}
