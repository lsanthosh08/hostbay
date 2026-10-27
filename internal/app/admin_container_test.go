//go:build !windows

package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/container"
)

// fakeDocker is a docker on this computer that logs each call's arguments
// to $HB_LOG; inspect prints a privileged container, exec runs cat so a
// terminal echoes what is typed.
const fakeDocker = `#!/bin/sh
echo "$@" >> "$HB_LOG"
case "$1" in
inspect) /bin/cat "$HB_INSPECT" ;;
exec) exec /bin/cat ;;
run) echo c0ffee ;;
esac
`

type askLog struct {
	answer   bool
	messages []string
	timed    int // confirmations whose wait had a deadline
}

func (a *askLog) confirm(ctx context.Context, title, message string) bool {
	a.messages = append(a.messages, title+": "+message)
	if _, ok := ctx.Deadline(); ok {
		a.timed++
	}
	return a.answer
}

// localAdmin opens a stand-in session for the fake docker on this
// computer, its socket given by DOCKER_HOST, and returns the service, the
// session ID, the confirmation's log and the docker call log.
func localAdmin(t *testing.T) (*AdminService, *ConnService, string, *askLog, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	t.Setenv("HB_LOG", log)
	t.Setenv("HB_INSPECT", "../../testdata/containers/docker/inspect-privileged.json")
	conns, _ := newConnService(t)
	a := NewAdminService(conns.mgr, conns)
	ask := &askLog{}
	a.confirm = ask.confirm
	id, err := a.ContainerOpenLocal(context.Background(), "docker")
	if err != nil {
		t.Fatal(err)
	}
	return a, conns, id, ask, log
}

func calls(t *testing.T, log string) string {
	b, _ := os.ReadFile(log)
	return string(b)
}

func TestLocalCreateConfirmsBindMounts(t *testing.T) {
	a, _, id, ask, log := localAdmin(t)
	ctx := context.Background()

	_, err := a.DockerCreate(ctx, id, container.CreateOptions{Image: "alpine", Binds: []container.MountOption{{Source: "/", Target: "/host"}}})
	if err == nil || len(ask.messages) != 1 || !strings.Contains(ask.messages[0], "/ → /host") || !strings.Contains(ask.messages[0], "this computer") {
		t.Fatalf("create = %v, asked %q", err, ask.messages)
	}
	if got := calls(t, log); got != "" {
		t.Fatalf("a refused create ran: %q", got)
	}

	ask.messages = nil
	cid, err := a.DockerCreate(ctx, id, container.CreateOptions{Image: "alpine", Volumes: []container.MountOption{{Source: "data", Target: "/data"}},
		Ports: []container.PortOption{{HostPort: 8080, ContainerPort: 80}}})
	if err != nil || cid != "c0ffee" || len(ask.messages) != 0 {
		t.Fatalf("create = %q, %v, asked %q", cid, err, ask.messages)
	}
	if got := calls(t, log); got != "run -d -p 127.0.0.1:8080:80 -v data:/data -- alpine\n" {
		t.Errorf("calls = %q", got)
	}
}

func TestLocalComposeAndExecAskFirst(t *testing.T) {
	a, conns, id, ask, log := localAdmin(t)
	ev := &termEvents{}
	conns.emit = ev.emit
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "compose.yml")

	if _, err := a.ComposeAction(ctx, id, file, "up"); err == nil || len(ask.messages) != 1 || !strings.Contains(ask.messages[0], file) {
		t.Fatalf("compose up = %v, asked %q", err, ask.messages)
	}
	if _, err := a.ContainerTerminal(ctx, "tab-1", id, "tools", "sh", false, 80, 24); err == nil {
		t.Fatal("a refused exec opened")
	}
	if !strings.Contains(ask.messages[1], "privileged") || !strings.Contains(ask.messages[1], "/var/run/docker.sock") {
		t.Errorf("exec asked %q", ask.messages[1])
	}
	if got := calls(t, log); got != "inspect -- tools\n" {
		t.Fatalf("calls = %q", got)
	}

	ask.answer = true
	pty, err := a.ContainerTerminal(ctx, "tab-1", id, "tools", "sh", false, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	conns.PtyAttach(pty)
	conns.PtyWrite(pty, "echo-me\n")
	// The terminal echoes the line, then cat inside the "container" sends it back.
	waitUntil(t, "the echo", func() bool { return strings.Count(ev.joined(PtyDataEvent(pty)), "echo-me") >= 2 })
	waitUntil(t, "the exec", func() bool { return strings.HasSuffix(calls(t, log), "exec -it -- tools sh\n") })
	conns.Disconnect("tab-1")
	waitUntil(t, "the closed event", func() bool { return ev.joined(PtyClosedEvent(pty)) != "" })
}

// A confirmation waits on the call alone, never on the engine command's
// time limit: reading the dialog must not use up the command's budget.
func TestLocalConfirmationsWaitOutsideTheExecTimeout(t *testing.T) {
	a, conns, id, ask, _ := localAdmin(t)
	ctx := context.Background()
	ask.answer = true
	if _, err := a.ComposeAction(ctx, id, filepath.Join(t.TempDir(), "compose.yml"), "up"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DockerCreate(ctx, id, container.CreateOptions{Image: "alpine", Binds: []container.MountOption{{Source: "/", Target: "/host"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ContainerTerminal(ctx, "tab-1", id, "tools", "sh", false, 80, 24); err != nil {
		t.Fatal(err)
	}
	conns.Disconnect("tab-1")
	if len(ask.messages) != 3 || ask.timed != 0 {
		t.Fatalf("%d of %d confirmations waited under the exec timeout", ask.timed, len(ask.messages))
	}
}

func TestLocalSessionIsNotRemote(t *testing.T) {
	a, _, id, _, _ := localAdmin(t)
	ctx := context.Background()
	if _, err := a.DockerFixPermission(ctx, id); err == nil {
		t.Error("fixed a local docker permission")
	}
	info, err := a.ContainerInfo(ctx, id)
	if err != nil || !info.Local || info.Endpoint != "unix:///run/user/1000/docker.sock" || info.Context != "DOCKER_HOST" || !strings.HasSuffix(info.Path, "/docker") {
		t.Errorf("info = %+v, %v", info, err)
	}
	a.ContainerClose(id)
	if _, err := a.DockerPs(ctx, id); err == nil {
		t.Error("a closed session still runs")
	}
}

func TestLocalRefusesARemoteEndpoint(t *testing.T) {
	conns, _ := newConnService(t)
	a := NewAdminService(conns.mgr, conns)
	t.Setenv("DOCKER_HOST", "ssh://dev@build.example.com")
	if _, err := a.ContainerOpenLocal(context.Background(), "docker"); err == nil || !strings.Contains(err.Error(), "ssh://dev@build.example.com") {
		t.Errorf("open = %v", err)
	}
}
