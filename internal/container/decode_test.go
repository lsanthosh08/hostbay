package container

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hostbay/internal/admin"
	"hostbay/internal/terminal"
)

// fixtures holds output recorded from each engine's CLI.
const fixtures = "../../testdata/containers"

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fakeRunner answers each command with out[its subcommand] and records it.
type fakeRunner struct {
	out   map[string]string
	calls [][]string
	stdin [][]byte
}

// sub is the first word after the engine's name and namespace flag.
func sub(argv []string) string {
	rest := argv[1:]
	if len(rest) > 1 && rest[0] == "--namespace" {
		rest = rest[2:]
	}
	return rest[0]
}

func (f *fakeRunner) Run(_ context.Context, argv []string, stdin []byte) (admin.Result, error) {
	f.calls = append(f.calls, argv)
	f.stdin = append(f.stdin, stdin)
	return admin.Result{Stdout: f.out[sub(argv)], Code: new(int)}, nil
}

func (f *fakeRunner) PTY(context.Context, []string, int, int) (terminal.Transport, error) {
	return nil, nil
}

func engine(t *testing.T, kind Kind, ns string, out map[string]string) (*Engine, *fakeRunner) {
	t.Helper()
	f := &fakeRunner{out: out}
	e, err := New(kind, ns, f)
	if err != nil {
		t.Fatal(err)
	}
	return e, f
}

func TestDockerAndPodmanListTheSameContainers(t *testing.T) {
	want := []Container{
		{Name: "web", Image: "docker.io/library/nginx:1.27", Status: "Up 2 hours", State: "running", Ports: "0.0.0.0:8080->80/tcp"},
		{Name: "db", Image: "docker.io/library/postgres:16", Status: "Exited (0) 3 hours ago", State: "exited"},
	}
	for _, c := range []struct {
		kind    Kind
		fixture string
	}{
		{Docker, "docker/ps.jsonl"},
		{Podman, "podman/4/ps.json"},
		{Podman, "podman/5/ps.json"},
	} {
		e, _ := engine(t, c.kind, "", map[string]string{"ps": fixture(t, c.fixture)})
		got, err := e.Ps(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", c.fixture, err)
		}
		for i := range got {
			if got[i].ID == "" {
				t.Errorf("%s: container %d has no ID", c.fixture, i)
			}
			got[i].ID = ""
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: ps = %+v", c.fixture, got)
		}
	}
}

func TestNerdctlAndWSLCStates(t *testing.T) {
	e, _ := engine(t, Nerdctl, "", map[string]string{"ps": fixture(t, "nerdctl/ps.jsonl")})
	got, err := e.Ps(context.Background())
	if err != nil || len(got) != 2 || got[0].State != "running" || got[1].State != "exited" || got[0].Ports != "0.0.0.0:8080->80/tcp" {
		t.Errorf("nerdctl ps = %+v, %v", got, err)
	}

	e, _ = engine(t, WSLC, "", map[string]string{"ps": fixture(t, "wslc/ps.json")})
	got, err = e.Ps(context.Background())
	if err != nil || len(got) != 3 {
		t.Fatalf("wslc ps = %+v, %v", got, err)
	}
	if got[1].Name != "web-app" || got[1].State != "running" || got[1].Ports != "127.0.0.1:18888->8888/tcp" || got[2].State != "exited" {
		t.Errorf("wslc ps = %+v", got)
	}
}

func TestUnknownOutputIsAnError(t *testing.T) {
	for _, out := range []string{fixture(t, "podman/ps-unknown.json"), "Error: something\n", `[{"Foo":1}]`} {
		e, _ := engine(t, Podman, "", map[string]string{"ps": out})
		if got, err := e.Ps(context.Background()); err == nil || err.Error() != "podman output not understood" {
			t.Errorf("ps of %q = %+v, %v", out, got, err)
		}
	}
	e, _ := engine(t, Podman, "", map[string]string{"ps": "[]\n"})
	if got, err := e.Ps(context.Background()); err != nil || len(got) != 0 {
		t.Errorf("empty ps = %+v, %v", got, err)
	}
}

func TestImagesAndStats(t *testing.T) {
	ctx := context.Background()
	nginx := Image{Repository: "docker.io/library/nginx", Tag: "1.27"}
	for _, c := range []struct {
		kind    Kind
		fixture string
		size    string
	}{
		{Docker, "docker/images.jsonl", "197MB"},
		{Podman, "podman/images.json", "197MB"},
		{Nerdctl, "nerdctl/images.jsonl", "196.9MB"},
	} {
		e, _ := engine(t, c.kind, "", map[string]string{"images": fixture(t, c.fixture)})
		got, err := e.Images(ctx)
		if err != nil || len(got) == 0 || got[0].ID == "" || got[0].Repository != nginx.Repository || got[0].Tag != nginx.Tag || got[0].Size != c.size {
			t.Errorf("%s: images = %+v, %v", c.fixture, got, err)
		}
		if c.kind != Nerdctl && (got[1].Repository != "<none>" || got[1].Tag != "<none>" || got[1].Size != "4.45MB") {
			t.Errorf("%s: untagged image = %+v", c.fixture, got[1])
		}
	}
	e, _ := engine(t, WSLC, "", map[string]string{"images": fixture(t, "wslc/images.json")})
	if got, err := e.Images(ctx); err != nil || len(got) != 2 || got[1].Repository != "redis" || got[1].Tag != "7" || got[1].Size != "52MB" {
		t.Errorf("wslc images = %+v, %v", got, err)
	}

	want := Stat{ID: "3f1c2a9b8d7e", Name: "web", CPU: "0.12%", MemPerc: "0.48%"}
	for kind, f := range map[Kind]string{Docker: "docker/stats.jsonl", Podman: "podman/stats.json"} {
		e, _ := engine(t, kind, "", map[string]string{"stats": fixture(t, f)})
		got, err := e.Stats(ctx)
		if err != nil || len(got) != 1 || got[0].Mem == "" {
			t.Fatalf("%s: stats = %+v, %v", f, got, err)
		}
		got[0].Mem = ""
		if got[0] != want {
			t.Errorf("%s: stats = %+v", f, got[0])
		}
	}
}

func TestNerdctlNamespaces(t *testing.T) {
	ctx := context.Background()
	e, f := engine(t, Nerdctl, "", map[string]string{"namespace": fixture(t, "nerdctl/namespaces.txt"), "ps": "", "images": ""})
	ns, err := e.Namespaces(ctx)
	if err != nil || !slices.Equal(ns, []string{"buildkit", "default", "k8s.io"}) {
		t.Fatalf("namespaces = %q, %v", ns, err)
	}

	e, err = New(Nerdctl, "k8s.io", f)
	if err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	e.Ps(ctx)
	e.Images(ctx)
	e.Action(ctx, "web", "stop")
	e.Logs(ctx, "web", "200")
	if len(f.calls) != 4 {
		t.Fatalf("calls = %q", f.calls)
	}
	for _, argv := range f.calls {
		if !slices.Equal(argv[:3], []string{"nerdctl", "--namespace", "k8s.io"}) {
			t.Errorf("argv %q has no --namespace k8s.io", argv)
		}
	}
	if _, err := New(Nerdctl, "a;b", f); err == nil {
		t.Error("namespace a;b accepted")
	}
	if _, err := New(Docker, "k8s.io", f); err == nil {
		t.Error("docker accepted a namespace")
	}
}

func TestArgumentsAreValidatedAndEndOptions(t *testing.T) {
	ctx := context.Background()
	e, f := engine(t, Docker, "", map[string]string{"inspect": fixture(t, "docker/inspect-privileged.json")})
	for _, bad := range []string{"--privileged", "-v", "web;reboot", "a b", ""} {
		if e.Action(ctx, bad, "stop") == nil {
			t.Errorf("container %q accepted", bad)
		}
		if _, err := e.Create(ctx, CreateOptions{Image: "nginx", Name: bad}); err == nil && bad != "" {
			t.Errorf("name %q accepted", bad)
		} else if bad == "" {
			f.calls = nil // no name is allowed
		}
		if _, err := e.Pull(ctx, bad); err == nil {
			t.Errorf("image %q accepted", bad)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("commands ran: %q", f.calls)
	}

	e.Action(ctx, "web", "start")
	e.Action(ctx, "web", "rm")
	e.Rmi(ctx, "sha256:ab12")
	e.Pull(ctx, "ghcr.io/org/app:1.2")
	e.Logs(ctx, "web", "all")
	e.ListDir(ctx, "web", "/etc")
	e.Inspect(ctx, "web")
	e.Login(ctx, "ghcr.io", "dev", "s3cret")
	e.Create(ctx, CreateOptions{Image: "nginx:1.27"})
	argv, _ := e.ExecArgv("web", "/bin/sh")
	want := [][]string{
		{"docker", "start", "--", "web"},
		{"docker", "rm", "-f", "--", "web"},
		{"docker", "rmi", "--", "sha256:ab12"},
		{"docker", "pull", "--", "ghcr.io/org/app:1.2"},
		{"docker", "logs", "-t", "--tail", "all", "--", "web"},
		{"docker", "exec", "--", "web", "ls", "-lA", "--", "/etc"},
		{"docker", "inspect", "--", "web"},
		{"docker", "login", "--password-stdin", "-u", "dev", "--", "ghcr.io"},
		{"docker", "run", "-d", "--", "nginx:1.27"},
		{"docker", "exec", "-it", "--", "web", "/bin/sh"},
	}
	got := append(f.calls, argv)
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("commands =\n%q\nwant\n%q", got, want)
	}

	// Podman's --tail is a number, -1 for every line; "all" fails to parse.
	p, pf := engine(t, Podman, "", nil)
	p.Logs(ctx, "web", "all")
	if want := []string{"podman", "logs", "-t", "--tail", "-1", "--", "web"}; len(pf.calls) != 1 || !slices.Equal(pf.calls[0], want) {
		t.Errorf("podman logs = %q, want %q", pf.calls, want)
	}
}

func TestExposure(t *testing.T) {
	x, err := decodeExposure(Docker, fixture(t, "docker/inspect-privileged.json"))
	if err != nil || !x.Privileged || !slices.Equal(x.Binds, []string{"/var/run/docker.sock"}) {
		t.Errorf("exposure = %+v, %v", x, err)
	}
	if _, err := decodeExposure(Docker, "[]"); err == nil {
		t.Error("empty inspect accepted")
	}
}

func TestMergeLogs(t *testing.T) {
	out := "2026-10-20T09:00:00.000000001Z a\n2026-10-20T09:00:02.000000000Z c\n"
	errs := "2026-10-20T09:00:01.500000000Z b\n"
	if got := mergeLogs(out, errs); got != strings.Join([]string{
		"2026-10-20T09:00:00.000000001Z a", "2026-10-20T09:00:01.500000000Z b", "2026-10-20T09:00:02.000000000Z c",
	}, "\n") {
		t.Errorf("merged = %q", got)
	}
}
