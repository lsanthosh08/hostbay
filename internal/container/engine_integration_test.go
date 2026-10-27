//go:build integration

package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"hostbay/internal/dockertest"
)

// Drives the Docker engine on this computer through a throwaway container,
// which the test removes, whenever a Docker daemon answers (as in the Linux
// CI integration job):
//
//	go test -tags integration -run LocalDocker ./internal/container/
//
// HOSTBAY_DOCKER_IMAGE picks the image (default nginx:alpine); it must
// keep running on its own and have sh.
func TestLocalDockerLifecycle(t *testing.T) {
	dockertest.Require(t)
	image := os.Getenv("HOSTBAY_DOCKER_IMAGE")
	if image == "" {
		image = "nginx:alpine"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	l, err := OpenLocal(ctx, Docker)
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(Docker, "", l.Runner)
	if err != nil {
		t.Fatal(err)
	}
	var b [4]byte
	rand.Read(b[:])
	name := "hostbay-it-" + hex.EncodeToString(b[:])
	if _, err := e.Create(ctx, CreateOptions{Image: image, Name: name, Env: []string{"HB=1"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Action(context.Background(), name, "rm") })

	for _, a := range []string{"stop", "start"} {
		if err := e.Action(ctx, name, a); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	ps, err := e.Ps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range ps {
		found = found || c.Name == name && c.State == "running"
	}
	if !found {
		t.Fatalf("%s is not running: %+v", name, ps)
	}
	if _, err := e.Logs(ctx, name, "50"); err != nil {
		t.Fatal(err)
	}

	argv, err := e.ExecArgv(name, "sh")
	if err != nil {
		t.Fatal(err)
	}
	term, err := e.Runner.PTY(ctx, argv, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(term, "echo ok-$((6*7)) $HB; exit\n")
	out, _ := io.ReadAll(term)
	term.Close()
	if !strings.Contains(string(out), "ok-42 1") {
		t.Errorf("exec output = %q", out)
	}

	if err := e.Action(ctx, name, "rm"); err != nil {
		t.Fatal(err)
	}
	ps, _ = e.Ps(ctx)
	for _, c := range ps {
		if c.Name == name {
			t.Errorf("%s still listed after rm", name)
		}
	}
}
