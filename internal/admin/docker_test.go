package admin

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/sftp/sftptest"
)

// testServer starts the in-process SSH server and logs in to it.
func testServer(t *testing.T) (*sftptest.Server, *ssh.Client) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	s := sftptest.New(t, "pw")
	return s, s.Dial(t)
}

// fakeBin puts an executable sh script called name first on the PATH the
// test server's commands see.
func fakeBin(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The engine commands' cases moved with them to internal/container.

func TestComposeProjects(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the search always includes ~; keep it off the real home
	root := t.TempDir()
	proj := filepath.Join(root, "my app")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(proj, "docker-compose.yml")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "pwned")
	srv, c := testServer(t)
	ctx := context.Background()

	got, err := ComposeProjects(ctx, c, []string{root, root, "/nowhere/$(touch " + marker + ")"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, ComposeProject{Name: "my app", Path: file}) {
		t.Errorf("projects = %+v", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a search path ran a command substitution")
	}
	if _, err := ComposeProjects(ctx, c, []string{"-delete"}); err == nil {
		t.Error("compose search path -delete accepted")
	}
	if n := len(srv.Commands()); n != 1 {
		t.Errorf("%d commands ran, want 1", n)
	}
}
