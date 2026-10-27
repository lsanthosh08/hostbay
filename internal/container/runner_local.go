package container

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"hostbay/internal/admin"
	"hostbay/internal/terminal"
)

// LocalRunner runs an engine on this computer: the binary at Path, with
// argv[1:] as its arguments and no shell, in environment Env.
type LocalRunner struct {
	Path string
	Env  []string
}

func (r LocalRunner) Run(ctx context.Context, argv []string, stdin []byte) (admin.Result, error) {
	cmd := exec.CommandContext(ctx, r.Path, argv[1:]...)
	cmd.Env = r.Env
	hideWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	if ctx.Err() != nil {
		return admin.Result{}, ctx.Err()
	}
	res := admin.Result{Stdout: stdout.String(), Stderr: stderr.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
		res.Code = new(int)
	case errors.As(err, &exit):
		if code := exit.ExitCode(); code >= 0 { // -1: ended by a signal
			res.Code = &code
		}
	default:
		return admin.Result{}, err
	}
	return res, nil
}

func (r LocalRunner) PTY(_ context.Context, argv []string, cols, rows int) (terminal.Transport, error) {
	return terminal.OpenCommand(r.Path, argv[1:], r.Env, cols, rows)
}

// installDirs are the folders searched after PATH, since an app started
// from the macOS Finder or a desktop menu gets a short PATH (KTD27).
var installDirs = map[string][]string{
	"darwin":  {"/usr/local/bin", "/opt/homebrew/bin", "/opt/podman/bin", "~/.docker/bin", "~/.rd/bin"},
	"linux":   {"~/.local/bin", "/usr/local/bin"},
	"windows": {`%ProgramFiles%\Docker\Docker\resources\bin`, `%ProgramFiles%\RedHat\Podman`, `%ProgramFiles%\WSL`},
}

// Find finds kind's binary on PATH, then in installDirs, and
// returns its absolute path. Relative PATH entries are skipped, and on
// Windows only an .exe counts, never a .bat or .cmd shim.
func Find(kind Kind) (string, error) {
	name := string(kind)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	home, _ := os.UserHomeDir()
	for _, dir := range append(filepath.SplitList(os.Getenv("PATH")), installDirs[runtime.GOOS]...) {
		if rest, ok := strings.CutPrefix(dir, "~/"); ok && home != "" {
			dir = filepath.Join(home, rest)
		}
		dir = os.ExpandEnv(strings.ReplaceAll(dir, "%ProgramFiles%", "${ProgramFiles}"))
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && (runtime.GOOS == "windows" || fi.Mode()&0o111 != 0) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s was not found on this computer", kind)
}

// guarded are the variables that could point an engine somewhere other
// than this computer. They are read only to resolve the endpoint and are
// removed from the child's environment, which then names the endpoint
// itself (KTD27).
var guarded = []string{"DOCKER_HOST", "DOCKER_CONTEXT", "CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINERD_ADDRESS"}

// childEnv is env without the guarded variables, with dir first on PATH so
// the engine's credential helpers beside it resolve.
func childEnv(env []string, dir string) []string {
	out := []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.EqualFold(k, "PATH") && !isGuarded(k) {
			out = append(out, kv)
		}
	}
	return out
}

func isGuarded(k string) bool {
	for _, g := range guarded {
		if strings.EqualFold(k, g) {
			return true
		}
	}
	return false
}

// Local is an engine on this computer, resolved once when its tab opens.
type Local struct {
	Kind     Kind
	Path     string // the binary
	Context  string // the Docker context or Podman connection used
	Endpoint string // the socket, or "" for the engine's own default
	Runner   LocalRunner
}

// OpenLocal resolves kind's binary and its endpoint on this computer, and
// refuses an endpoint that is not a local socket.
func OpenLocal(ctx context.Context, kind Kind) (Local, error) {
	if _, ok := engines[kind]; !ok {
		return Local{}, fmt.Errorf("unknown container engine: %q", kind)
	}
	if kind == WSLC && runtime.GOOS != "windows" {
		return Local{}, errors.New("WSLC runs only on Windows")
	}
	path, err := Find(kind)
	if err != nil {
		return Local{}, err
	}
	l := Local{Kind: kind, Path: path, Runner: LocalRunner{Path: path, Env: childEnv(os.Environ(), filepath.Dir(path))}}
	name, endpoint, err := l.endpoint(ctx)
	if err != nil {
		return Local{}, err
	}
	l.Context, l.Endpoint = name, endpoint
	if v := map[Kind]string{Docker: "DOCKER_HOST", Podman: "CONTAINER_HOST", Nerdctl: "CONTAINERD_ADDRESS"}[kind]; v != "" && endpoint != "" {
		l.Runner.Env = append(l.Runner.Env, v+"="+endpoint)
	}
	return l, nil
}

// endpoint finds the socket the engine would use: Docker's DOCKER_HOST or
// current context, Podman's CONTAINER_HOST, CONTAINER_CONNECTION or default
// connection (a machine's resolving to its API socket), nerdctl's
// CONTAINERD_ADDRESS. "" leaves the engine on its own local default.
func (l Local) endpoint(ctx context.Context) (name, endpoint string, err error) {
	switch l.Kind {
	case Docker:
		if h := os.Getenv("DOCKER_HOST"); h != "" {
			return "DOCKER_HOST", h, localSocket(h)
		}
		args := []string{"context", "inspect"}
		if c := os.Getenv("DOCKER_CONTEXT"); c != "" {
			if err := admin.ValidToken(c, "_.-"); err != nil {
				return "", "", err
			}
			args = append(args, "--", c)
		}
		var ctxs []struct {
			Name      string
			Endpoints struct{ Docker struct{ Host string } }
		}
		if err := l.probe(ctx, &ctxs, args...); err != nil || len(ctxs) == 0 {
			return "", "", fmt.Errorf("could not read the Docker context: %v", err)
		}
		h := ctxs[0].Endpoints.Docker.Host
		return ctxs[0].Name, h, localSocket(h)
	case Podman:
		if h := os.Getenv("CONTAINER_HOST"); h != "" {
			return "CONTAINER_HOST", h, localSocket(h)
		}
		want := os.Getenv("CONTAINER_CONNECTION")
		if want == "" && runtime.GOOS == "linux" {
			return "", "", nil // Podman on Linux runs containers itself, with no service
		}
		var conns []struct {
			Name, URI          string
			Default, IsMachine bool
		}
		if err := l.probe(ctx, &conns, "system", "connection", "list", "--format", "json"); err != nil {
			return "", "", fmt.Errorf("could not read the Podman connections: %v", err)
		}
		for _, c := range conns {
			if want != "" && c.Name != want || want == "" && !c.Default {
				continue
			}
			if u, err := url.Parse(c.URI); err == nil && u.Scheme == "ssh" && (c.IsMachine || isLoopback(u.Hostname())) {
				sock, err := l.machineSocket(ctx, strings.TrimSuffix(c.Name, "-root"))
				return c.Name, sock, err
			}
			return c.Name, c.URI, localSocket(c.URI)
		}
		if want != "" {
			return "", "", fmt.Errorf("Podman has no connection named %q", want)
		}
		return "", "", nil
	case Nerdctl:
		a := os.Getenv("CONTAINERD_ADDRESS")
		if a == "" {
			return "", "", nil
		}
		p := strings.TrimPrefix(a, "unix://")
		if !filepath.IsAbs(p) {
			return "", "", fmt.Errorf("%s is not a socket on this computer", a)
		}
		return "CONTAINERD_ADDRESS", p, nil
	}
	return "", "", nil
}

// machineSocket is the API socket `podman machine inspect` reports for a
// Podman machine, so its ssh:// connection is used as the local socket it
// forwards.
func (l Local) machineSocket(ctx context.Context, machine string) (string, error) {
	if err := admin.ValidToken(machine, "_.-"); err != nil {
		return "", err
	}
	var ms []struct {
		ConnectionInfo struct{ PodmanSocket, PodmanPipe *struct{ Path string } }
	}
	if err := l.probe(ctx, &ms, "machine", "inspect", "--", machine); err != nil || len(ms) == 0 {
		return "", fmt.Errorf("could not read the Podman machine %q: %v", machine, err)
	}
	ci := ms[0].ConnectionInfo
	switch {
	case ci.PodmanPipe != nil && ci.PodmanPipe.Path != "" && runtime.GOOS == "windows":
		return "npipe://" + filepath.ToSlash(ci.PodmanPipe.Path), nil
	case ci.PodmanSocket != nil && ci.PodmanSocket.Path != "":
		return "unix://" + ci.PodmanSocket.Path, nil
	}
	return "", fmt.Errorf("the Podman machine %q reports no API socket", machine)
}

// probe runs a read-only engine command and decodes its JSON into v.
func (l Local) probe(ctx context.Context, v any, args ...string) error {
	r, err := l.Runner.Run(ctx, append([]string{string(l.Kind)}, args...), nil)
	if err != nil {
		return err
	}
	if r.Code == nil || *r.Code != 0 {
		return errors.New(strings.TrimSpace(r.Stderr))
	}
	return json.Unmarshal([]byte(r.Stdout), v)
}

// localSocket accepts only a unix:// socket path or a Windows named pipe.
func localSocket(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err == nil && (u.Scheme == "unix" && u.Host == "" && filepath.IsAbs(u.Path) ||
		u.Scheme == "npipe" && runtime.GOOS == "windows") {
		return nil
	}
	return fmt.Errorf("%s is not a socket on this computer; Hostbay manages only engines on this computer here, so connect to that host over SSH instead", endpoint)
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
