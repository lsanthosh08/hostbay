package container

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx"
)

// remote connects to the in-process SSH server and returns a Docker
// engine on its lease. The server runs commands with /bin/sh.
func remote(t *testing.T) (*sftptest.Server, *Engine) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	s := sftptest.New(t, "pw")
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(s.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(s.Addr)
	p, _ := strconv.Atoi(port)
	mgr := sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh})
	l, err := mgr.Acquire(context.Background(), sshx.Target{Key: "h1", Hops: []sshx.Hop{
		{Host: host, Port: p, User: "u", Auth: sshx.Auth{Password: s.Password}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Release)
	e, err := New(Docker, "", SSHRunner{Lease: l})
	if err != nil {
		t.Fatal(err)
	}
	return s, e
}

func TestRemoteDockerCommands(t *testing.T) {
	s, e := remote(t)
	s.Command(t, "docker", `#!/bin/sh
case "$1" in
ps) echo '{"ID":"abc","Names":"web","Image":"nginx","Status":"Up 2 hours","State":"running","Ports":"80/tcp"}'
    echo 'WARNING: not json' ;;
login) cat > "$HB_STDIN" ;;
esac
`)
	stdin := filepath.Join(t.TempDir(), "stdin")
	t.Setenv("HB_STDIN", stdin)
	ctx := context.Background()
	for _, a := range []string{"start", "stop", "restart", "rm"} {
		if err := e.Action(ctx, "web_1", a); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	ps, err := e.Ps(ctx)
	if err != nil || !slices.Equal(ps, []Container{{ID: "abc", Name: "web", Image: "nginx", Status: "Up 2 hours", State: "running", Ports: "80/tcp"}}) {
		t.Errorf("ps = %+v, %v", ps, err)
	}
	if err := e.Login(ctx, "ghcr.io", "dev", "it's s3cret"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ComposeAction(ctx, "/srv/my app/compose.yml", "up"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"docker start -- web_1", "docker stop -- web_1", "docker restart -- web_1", "docker rm -f -- web_1",
		"docker ps -a --format '{{json .}}'",
		"docker login --password-stdin -u dev -- ghcr.io",
		"docker compose -f '/srv/my app/compose.yml' up -d",
	}
	if got := s.Commands(); !slices.Equal(got, want) {
		t.Errorf("commands =\n%q\nwant\n%q", got, want)
	}
	if b, _ := os.ReadFile(stdin); string(b) != "it's s3cret\n" {
		t.Errorf("login stdin = %q", b)
	}
}

// The UI offers the docker-group fix when listing fails with a permission
// error, so a failed docker call must come back as an error, not an empty
// list as docker.rs returned.
func TestRemoteFailureIsError(t *testing.T) {
	s, e := remote(t)
	s.Command(t, "docker", "#!/bin/sh\necho 'permission denied while trying to connect to the Docker daemon socket' >&2; exit 1\n")
	ctx := context.Background()
	if _, err := e.Ps(ctx); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("ps err = %v", err)
	}
	if err := e.Action(ctx, "web", "stop"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("action err = %v", err)
	}
}

func TestCommandQuoting(t *testing.T) {
	got := Command([]string{"docker", "exec", "--", "web", "ls", "-lA", "--", "/a b/it's", ""})
	if want := `docker exec -- web ls -lA -- '/a b/it'\''s' ''`; got != want {
		t.Errorf("command = %s, want %s", got, want)
	}
}
