//go:build !windows

package container

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeEngine installs an executable sh script as the only binary of name
// Hostbay can find, and points HB_LOG at the file it records into.
func fakeEngine(t *testing.T, name, script string) (dir, log string) {
	t.Helper()
	dir = t.TempDir()
	log = filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := installDirs[runtime.GOOS]
	installDirs[runtime.GOOS] = []string{dir}
	t.Cleanup(func() { installDirs[runtime.GOOS] = old })
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HB_LOG", log)
	t.Setenv("HB_FIXTURES", fixtures)
	for _, g := range guarded {
		t.Setenv(g, "")
		os.Unsetenv(g)
	}
	return dir, log
}

// recorder logs each call's arguments and environment, then answers from
// the fixtures by subcommand.
const recorder = `echo "$@" >> "$HB_LOG.args"
/usr/bin/env > "$HB_LOG.env"
/bin/cat > "$HB_LOG.stdin"
case "$1 $2" in
"context inspect") /bin/cat "$HB_FIXTURES/docker/$HB_CONTEXT" ;;
"system connection") /bin/cat "$HB_FIXTURES/podman/connections.json" ;;
"machine inspect") /bin/cat "$HB_FIXTURES/podman/machine-inspect.json" ;;
esac
`

func read(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// env reads the recorded environment as name -> value.
func env(t *testing.T, log string) map[string]string {
	m := map[string]string{}
	for _, kv := range strings.Split(read(t, log+".env"), "\n") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

func noGuarded(t *testing.T, log string, except string) {
	t.Helper()
	for k := range env(t, log) {
		if isGuarded(k) && k != except {
			t.Errorf("the child sees %s", k)
		}
	}
}

func TestLocalRefusesARemoteDockerHost(t *testing.T) {
	_, log := fakeEngine(t, "docker", recorder)
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.1:2375")
	_, err := OpenLocal(context.Background(), Docker)
	if err == nil || !strings.Contains(err.Error(), "tcp://10.0.0.1:2375") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(log + ".args"); err == nil {
		t.Errorf("a child started: %q", read(t, log+".args"))
	}
}

func TestLocalUsesTheDockerHostSocket(t *testing.T) {
	_, log := fakeEngine(t, "docker", recorder)
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	t.Setenv("CONTAINER_HOST", "tcp://10.0.0.9:1")
	t.Setenv("CONTAINERD_ADDRESS", "/elsewhere.sock")
	l, err := OpenLocal(context.Background(), Docker)
	if err != nil || l.Endpoint != "unix:///run/user/1000/docker.sock" {
		t.Fatalf("local = %+v, %v", l, err)
	}
	e, _ := New(Docker, "", l.Runner)
	e.Ps(context.Background())
	if got := env(t, log)["DOCKER_HOST"]; got != l.Endpoint {
		t.Errorf("child DOCKER_HOST = %q", got)
	}
	noGuarded(t, log, "DOCKER_HOST")
}

func TestLocalUsesTheCurrentDockerContext(t *testing.T) {
	_, log := fakeEngine(t, "docker", recorder)
	t.Setenv("HB_CONTEXT", "context-desktop-linux.json")
	t.Setenv("DOCKER_CONTEXT", "desktop-linux")
	l, err := OpenLocal(context.Background(), Docker)
	if err != nil || l.Context != "desktop-linux" || l.Endpoint != "unix:///Users/dev/.docker/run/docker.sock" {
		t.Fatalf("local = %+v, %v", l, err)
	}
	if got := read(t, log+".args"); got != "context inspect -- desktop-linux\n" {
		t.Errorf("probe args = %q", got)
	}
	e, _ := New(Docker, "", l.Runner)
	e.Ps(context.Background())
	if got := env(t, log)["DOCKER_HOST"]; got != l.Endpoint {
		t.Errorf("child DOCKER_HOST = %q", got)
	}
	noGuarded(t, log, "DOCKER_HOST")

	t.Setenv("HB_CONTEXT", "context-ssh.json")
	os.Unsetenv("DOCKER_CONTEXT")
	if _, err := OpenLocal(context.Background(), Docker); err == nil || !strings.Contains(err.Error(), "ssh://dev@build.example.com") {
		t.Errorf("ssh context: %v", err)
	}
}

func TestLocalResolvesAPodmanMachine(t *testing.T) {
	_, log := fakeEngine(t, "podman", recorder)
	t.Setenv("CONTAINER_CONNECTION", "podman-machine-default")
	l, err := OpenLocal(context.Background(), Podman)
	want := "unix:///var/folders/xy/abc123/T/podman/podman-machine-default-api.sock"
	if err != nil || l.Context != "podman-machine-default" || l.Endpoint != want {
		t.Fatalf("local = %+v, %v", l, err)
	}
	if got := read(t, log+".args"); !strings.HasSuffix(got, "machine inspect -- podman-machine-default\n") {
		t.Errorf("probe args = %q", got)
	}
	e, _ := New(Podman, "", l.Runner)
	e.Ps(context.Background())
	if got := env(t, log)["CONTAINER_HOST"]; got != want {
		t.Errorf("child CONTAINER_HOST = %q", got)
	}
	noGuarded(t, log, "CONTAINER_HOST")

	t.Setenv("CONTAINER_CONNECTION", "build-server")
	if _, err := OpenLocal(context.Background(), Podman); err == nil || !strings.Contains(err.Error(), "ssh://dev@build.example.com") {
		t.Errorf("remote connection: %v", err)
	}
}

func TestLocalFindsAnEngineOffPATH(t *testing.T) {
	dir, log := fakeEngine(t, "docker", recorder)
	t.Setenv("HB_CONTEXT", "context-desktop-linux.json")
	// A short GUI PATH; when this machine has a docker there, the fake must
	// still win from its install folder, so leave PATH empty instead.
	path := "/usr/bin:/bin"
	if _, err := os.Stat("/usr/bin/docker"); err == nil {
		path = t.TempDir()
	}
	t.Setenv("PATH", path)
	l, err := OpenLocal(context.Background(), Docker)
	if err != nil || l.Path != filepath.Join(dir, "docker") {
		t.Fatalf("local = %+v, %v", l, err)
	}
	e, _ := New(Docker, "", l.Runner)
	e.Ps(context.Background())
	if got := env(t, log)["PATH"]; !strings.HasPrefix(got, dir+":") {
		t.Errorf("child PATH = %q", got)
	}
}

func TestLoginPasswordGoesOnStdinOnly(t *testing.T) {
	_, log := fakeEngine(t, "docker", recorder)
	t.Setenv("HB_CONTEXT", "context-desktop-linux.json")
	l, err := OpenLocal(context.Background(), Docker)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := New(Docker, "", l.Runner)
	if err := e.Login(context.Background(), "ghcr.io", "dev", "s3cret-pw"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, log+".stdin"); got != "s3cret-pw\n" {
		t.Errorf("stdin = %q", got)
	}
	for _, f := range []string{".args", ".env"} {
		if strings.Contains(read(t, log+f), "s3cret-pw") {
			t.Errorf("the password is in the child's %s", f)
		}
	}
}

func TestLocalRunnerReportsTheExitCode(t *testing.T) {
	dir, _ := fakeEngine(t, "docker", "echo out; echo err >&2; exit 3\n")
	r := LocalRunner{Path: filepath.Join(dir, "docker"), Env: os.Environ()}
	res, err := r.Run(context.Background(), []string{"docker", "x"}, nil)
	if err != nil || res.Code == nil || *res.Code != 3 || res.Stdout != "out\n" || res.Stderr != "err\n" {
		t.Errorf("result = %+v, %v", res, err)
	}
	if !slices.Contains(Kinds, Docker) {
		t.Fatal("docker is not a kind")
	}
}
