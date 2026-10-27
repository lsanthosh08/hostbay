package container

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"hostbay/internal/admin"
)

// Kind names an engine by its CLI.
type Kind string

const (
	Docker  Kind = "docker"
	Podman  Kind = "podman"
	Nerdctl Kind = "nerdctl"
	WSLC    Kind = "wslc"
)

// Kinds lists the engines in the order a picker offers them.
var Kinds = []Kind{Docker, Podman, Nerdctl, WSLC}

// Caps says which features an engine's CLI has, so the tab offers only those.
type Caps struct {
	Restart       bool `json:"restart"`       // a restart action
	RestartPolicy bool `json:"restartPolicy"` // run --restart
	Compose       bool `json:"compose"`
	Prune         bool `json:"prune"`
	DiskUsage     bool `json:"diskUsage"` // system df
	Namespaces    bool `json:"namespaces"`
}

// descriptor is what differs between engines beyond their decoders.
type descriptor struct {
	caps Caps
	json []string // the flags that make a list command print JSON
}

// WSLC's --format takes only the word json, and it has no restart, compose,
// prune or df (uniterm's commands.go).
var engines = map[Kind]descriptor{
	Docker:  {Caps{Restart: true, RestartPolicy: true, Compose: true, Prune: true, DiskUsage: true}, []string{"--format", "{{json .}}"}},
	Podman:  {Caps{Restart: true, RestartPolicy: true, Compose: true, Prune: true, DiskUsage: true}, []string{"--format", "json"}},
	Nerdctl: {Caps{Restart: true, RestartPolicy: true, Compose: true, Prune: true, Namespaces: true}, []string{"--format", "{{json .}}"}},
	WSLC:    {Caps{}, []string{"--format", "json"}},
}

// Engine is one engine reached through a Runner. Namespace is nerdctl's
// containerd namespace, passed on every command.
type Engine struct {
	Kind      Kind
	Namespace string
	Runner    Runner
}

// New checks kind and namespace; nerdctl's namespace defaults to "default".
func New(kind Kind, namespace string, r Runner) (*Engine, error) {
	d, ok := engines[kind]
	if !ok {
		return nil, fmt.Errorf("unknown container engine: %q", kind)
	}
	switch {
	case !d.caps.Namespaces && namespace != "":
		return nil, fmt.Errorf("%s has no namespaces", kind)
	case d.caps.Namespaces && namespace == "":
		namespace = "default"
	case d.caps.Namespaces:
		if err := admin.ValidToken(namespace, "_.-"); err != nil {
			return nil, err
		}
	}
	return &Engine{Kind: kind, Namespace: namespace, Runner: r}, nil
}

func (e *Engine) Caps() Caps { return engines[e.Kind].caps }

// argv puts the engine's name and nerdctl's namespace flag before args.
func (e *Engine) argv(args ...string) []string {
	out := []string{string(e.Kind)}
	if e.Namespace != "" {
		out = append(out, "--namespace", e.Namespace)
	}
	return append(out, args...)
}

func (e *Engine) list(args ...string) []string { return append(args, engines[e.Kind].json...) }

// exec runs args and returns the result whatever the exit code, for calls
// whose output the UI shows (pull, prune, compose).
func (e *Engine) exec(ctx context.Context, args ...string) (admin.Result, error) {
	return e.Runner.Run(ctx, e.argv(args...), nil)
}

// run is exec for calls the UI sees as success or failure: a non-zero exit
// becomes an error carrying the command's message.
func (e *Engine) run(ctx context.Context, stdin []byte, args ...string) (admin.Result, error) {
	argv := e.argv(args...)
	r, err := e.Runner.Run(ctx, argv, stdin)
	if err != nil || (r.Code != nil && *r.Code == 0) {
		return r, err
	}
	msg := strings.TrimSpace(r.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(r.Stdout)
	}
	if msg == "" {
		msg = "command failed: " + Command(argv)
	}
	return r, errors.New(msg)
}

func validID(id string) error   { return admin.ValidToken(id, "_-./:") }
func validImage(s string) error { return admin.ValidToken(s, "_./:@-") }
func validShell(s string) error { return admin.ValidToken(s, "_-./") }
func validInPath(p string) error { // a path inside a container
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\n\r") {
		return fmt.Errorf("illegal path: %q", p)
	}
	return nil
}

func (e *Engine) Ps(ctx context.Context) ([]Container, error) {
	r, err := e.run(ctx, nil, e.list("ps", "-a")...)
	if err != nil {
		return nil, err
	}
	return decodeContainers(e.Kind, r.Stdout)
}

func (e *Engine) Action(ctx context.Context, id, action string) error {
	switch action {
	case "start", "stop", "rm":
	case "restart":
		if !e.Caps().Restart {
			return fmt.Errorf("%s has no restart", e.Kind)
		}
	default:
		return fmt.Errorf("invalid action: %s", action)
	}
	if err := validID(id); err != nil {
		return err
	}
	args := []string{action, "--", id}
	if action == "rm" && e.Kind != WSLC { // uniterm's wslc rm takes no -f
		args = []string{"rm", "-f", "--", id}
	}
	_, err := e.run(ctx, nil, args...)
	return err
}

func (e *Engine) Images(ctx context.Context) ([]Image, error) {
	r, err := e.run(ctx, nil, e.list("images")...)
	if err != nil {
		return nil, err
	}
	return decodeImages(e.Kind, r.Stdout)
}

func (e *Engine) Pull(ctx context.Context, image string) (admin.Result, error) {
	if err := validImage(image); err != nil {
		return admin.Result{}, err
	}
	return e.exec(ctx, "pull", "--", image)
}

func (e *Engine) Rmi(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	_, err := e.run(ctx, nil, "rmi", "--", id)
	return err
}

func (e *Engine) Prune(ctx context.Context, what string) (admin.Result, error) {
	args, ok := map[string][]string{
		"dangling": {"image", "prune", "-f"},
		"all":      {"image", "prune", "-af"},
		"system":   {"system", "prune", "-af"},
	}[what]
	if !ok {
		return admin.Result{}, fmt.Errorf("invalid prune target: %s", what)
	}
	if !e.Caps().Prune {
		return admin.Result{}, fmt.Errorf("%s has no prune", e.Kind)
	}
	return e.exec(ctx, args...)
}

func (e *Engine) Stats(ctx context.Context) ([]Stat, error) {
	args := []string{"stats", "--no-stream"}
	if e.Kind == WSLC { // uniterm's wslc stats prints once without it
		args = args[:1]
	}
	r, err := e.run(ctx, nil, e.list(args...)...)
	if err != nil {
		return nil, err
	}
	return decodeStats(e.Kind, r.Stdout)
}

func (e *Engine) SystemDf(ctx context.Context) (string, error) {
	if !e.Caps().DiskUsage {
		return "", fmt.Errorf("%s has no disk usage report", e.Kind)
	}
	r, err := e.run(ctx, nil, "system", "df")
	return r.Stdout, err
}

// logArgs builds a logs command with timestamps, the last tail lines
// ("all" for every line), following when follow is set.
func (e *Engine) logArgs(id, tail string, follow bool) ([]string, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	if tail != "all" && (tail == "" || strings.Trim(tail, "0123456789") != "") {
		return nil, fmt.Errorf("invalid tail: %q", tail)
	}
	n := "--tail"
	if e.Kind == WSLC {
		n = "-n"
	}
	if tail == "all" && e.Kind == Podman {
		tail = "-1" // Podman's --tail is a number
	}
	args := []string{"logs", "-t", n, tail}
	if follow {
		args = append(args, "-f")
	}
	return e.argv(append(args, "--", id)...), nil
}

// Logs returns a container's last tail lines. The engine writes the
// container's stdout and stderr to its own two streams; they come back
// merged in timestamp order, as `2>&1` gave them before.
func (e *Engine) Logs(ctx context.Context, id, tail string) (string, error) {
	argv, err := e.logArgs(id, tail, false)
	if err != nil {
		return "", err
	}
	r, err := e.Runner.Run(ctx, argv, nil)
	if err != nil {
		return "", err
	}
	if r.Code == nil || *r.Code != 0 {
		return "", errors.New(strings.TrimSpace(r.Stderr + "\n" + r.Stdout))
	}
	return mergeLogs(r.Stdout, r.Stderr), nil
}

// FollowArgv is the command that follows a container's logs on a terminal.
func (e *Engine) FollowArgv(id string) ([]string, error) { return e.logArgs(id, "200", true) }

// ExecArgv is the command that opens shell inside a container on a terminal.
func (e *Engine) ExecArgv(id, shell string) ([]string, error) {
	if err := validID(id); err != nil {
		return nil, err
	}
	if err := validShell(shell); err != nil {
		return nil, err
	}
	return e.argv("exec", "-it", "--", id, shell), nil
}

// ListDir runs ls -lA on a path inside a container; ReadFile reads the
// first 256 KiB of a file there. Neither goes through a shell.
func (e *Engine) ListDir(ctx context.Context, id, path string) (string, error) {
	return e.inside(ctx, id, path, "ls", "-lA")
}

func (e *Engine) ReadFile(ctx context.Context, id, path string) (string, error) {
	return e.inside(ctx, id, path, "head", "-c", "262144")
}

func (e *Engine) inside(ctx context.Context, id, path string, cmd ...string) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}
	if err := validInPath(path); err != nil {
		return "", err
	}
	args := append(append([]string{"exec", "--", id}, cmd...), "--", path)
	r, err := e.run(ctx, nil, args...)
	return r.Stdout, err
}

// Exposure is what a container can reach on its host, from inspect.
type Exposure struct {
	Privileged bool
	Binds      []string // host paths bind-mounted into it
}

// Inspect reads a container's privileged flag and bind mounts.
func (e *Engine) Inspect(ctx context.Context, id string) (Exposure, error) {
	if err := validID(id); err != nil {
		return Exposure{}, err
	}
	r, err := e.run(ctx, nil, "inspect", "--", id)
	if err != nil {
		return Exposure{}, err
	}
	return decodeExposure(e.Kind, r.Stdout)
}

// Login signs in to a registry, sending the password once on stdin. The
// engine's credential store keeps it; Hostbay does not.
func (e *Engine) Login(ctx context.Context, registry, user, password string) error {
	if registry != "" {
		if err := admin.ValidToken(registry, "_.-:/"); err != nil {
			return err
		}
	}
	if err := admin.ValidToken(user, "_.-@+"); err != nil {
		return err
	}
	if password == "" || strings.ContainsAny(password, "\r\n") {
		return errors.New("the password is empty or has a line break")
	}
	args := []string{"login", "--password-stdin", "-u", user}
	if registry != "" {
		args = append(args, "--", registry)
	}
	_, err := e.run(ctx, []byte(password+"\n"), args...)
	return err
}

// Namespaces lists nerdctl's containerd namespaces.
func (e *Engine) Namespaces(ctx context.Context) ([]string, error) {
	if !e.Caps().Namespaces {
		return nil, fmt.Errorf("%s has no namespaces", e.Kind)
	}
	r, err := e.run(ctx, nil, "namespace", "ls", "-q")
	if err != nil {
		return nil, err
	}
	var ns []string
	for _, l := range strings.Split(r.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" && admin.ValidToken(l, "_.-") == nil {
			ns = append(ns, l)
		}
	}
	slices.Sort(ns)
	return ns, nil
}

// ComposeAction runs a Compose action on the project of file.
func (e *Engine) ComposeAction(ctx context.Context, file, action string) (admin.Result, error) {
	if err := admin.ValidComposePath(file); err != nil {
		return admin.Result{}, err
	}
	args, ok := map[string][]string{
		"up": {"up", "-d"}, "down": {"down"}, "restart": {"restart"}, "ps": {"ps"}, "logs": {"logs", "--tail", "200"},
	}[action]
	if !ok {
		return admin.Result{}, fmt.Errorf("invalid action: %s", action)
	}
	if !e.Caps().Compose {
		return admin.Result{}, fmt.Errorf("%s has no compose", e.Kind)
	}
	return e.exec(ctx, append([]string{"compose", "-f", file}, args...)...)
}
