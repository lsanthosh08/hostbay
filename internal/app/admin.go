package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/admin"
	"hostbay/internal/container"
	"hostbay/internal/sshx"
	"hostbay/internal/terminal"
	"hostbay/internal/wol"
)

// AdminService runs the server admin tabs (Docker, services, nginx, IIS,
// dashboards) and Wake-on-LAN. sessionID is the lease ID that
// ConnService.Connect returned, or for the Containers tab the ID of a
// stand-in session on this computer from ContainerOpenLocal. Each call
// gives up after admin.ExecTimeout.
type AdminService struct {
	mgr     *sshx.Manager
	conns   *ConnService
	confirm container.Confirm

	mu      sync.Mutex
	engines map[string]*engineChoice // session ID -> its Containers tab's engine
}

// engineChoice is the engine a session drives. On an SSH session it is
// the kind and namespace picked in the tab; on this computer it also holds
// the runner resolved when the engine was opened.
type engineChoice struct {
	id     string // the ContainerEngine ID
	kind   container.Kind
	ns     string
	runner container.Runner // nil on an SSH session
	local  *container.Local // nil inside WSL
}

func NewAdminService(mgr *sshx.Manager, conns *ConnService) *AdminService {
	return &AdminService{mgr: mgr, conns: conns, confirm: container.NativeConfirm, engines: map[string]*engineChoice{}}
}

// client returns the session's connection, rebuilding a dropped one first,
// and ctx bounded by admin.ExecTimeout.
func (s *AdminService) client(ctx context.Context, sessionID string) (*ssh.Client, context.Context, context.CancelFunc, error) {
	l := s.mgr.Lease(sessionID)
	if l == nil {
		return nil, nil, nil, errors.New("the session has ended; connect again")
	}
	cl, err := l.Live(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, admin.ExecTimeout)
	return cl.Client, ctx, cancel, nil
}

// call runs one admin function on the session's connection.
func call[T any](s *AdminService, ctx context.Context, sessionID string, f func(context.Context, *ssh.Client) (T, error)) (T, error) {
	c, ctx, cancel, err := s.client(ctx, sessionID)
	if err != nil {
		var zero T
		return zero, err
	}
	defer cancel()
	return f(ctx, c)
}

// callExec is call for the functions returning admin.Result.
func callExec(s *AdminService, ctx context.Context, sessionID string, f func(context.Context, *ssh.Client) (admin.Result, error)) (ExecResult, error) {
	r, err := call(s, ctx, sessionID, f)
	return ExecResult(r), err
}

// conv converts a slice between two struct types with the same fields.
func conv[U, T any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

// engine returns the session's container engine, Docker unless the tab
// picked another, and whether it runs on this computer.
func (s *AdminService) engine(sessionID string) (*container.Engine, bool, error) {
	s.mu.Lock()
	c := s.engines[sessionID]
	s.mu.Unlock()
	if c != nil && c.runner != nil {
		e, err := container.New(c.kind, c.ns, c.runner)
		return e, true, err
	}
	l := s.mgr.Lease(sessionID)
	if l == nil {
		s.mu.Lock()
		delete(s.engines, sessionID)
		s.mu.Unlock()
		return nil, false, errors.New("the session has ended; connect again")
	}
	kind, ns := container.Docker, ""
	if c != nil {
		kind, ns = c.kind, c.ns
	}
	e, err := container.New(kind, ns, container.SSHRunner{Lease: l})
	return e, false, err
}

// onEngine runs f on the session's engine within admin.ExecTimeout.
func onEngine[T any](s *AdminService, ctx context.Context, sessionID string, f func(context.Context, *container.Engine, bool) (T, error)) (T, error) {
	e, local, err := s.engine(sessionID)
	if err != nil {
		var zero T
		return zero, err
	}
	ctx, cancel := execCtx(ctx)
	defer cancel()
	return f(ctx, e, local)
}

var errCancelled = errors.New("cancelled")

// ask opens the native confirmation; a no ends the call with errCancelled.
// It waits on the call's own ctx, so only the user or the app quitting ends
// it; the action's admin.ExecTimeout starts after a yes.
func (s *AdminService) ask(ctx context.Context, title, message string) error {
	if !s.confirm(ctx, title, message) {
		return errCancelled
	}
	return nil
}

// execCtx bounds one engine command by admin.ExecTimeout.
func execCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, admin.ExecTimeout)
}

func (s *AdminService) DockerPs(ctx context.Context, sessionID string) ([]DockerContainer, error) {
	l, err := onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) ([]container.Container, error) {
		return e.Ps(ctx)
	})
	return conv(l, func(v container.Container) DockerContainer { return DockerContainer(v) }), err
}

func (s *AdminService) DockerAction(ctx context.Context, sessionID, id, action string) error {
	_, err := onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) (any, error) {
		return nil, e.Action(ctx, id, action)
	})
	return err
}

func (s *AdminService) DockerImages(ctx context.Context, sessionID string) ([]DockerImage, error) {
	l, err := onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) ([]container.Image, error) {
		return e.Images(ctx)
	})
	return conv(l, func(v container.Image) DockerImage { return DockerImage(v) }), err
}

func (s *AdminService) DockerPull(ctx context.Context, sessionID, image string) (ExecResult, error) {
	r, err := onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) (admin.Result, error) {
		return e.Pull(ctx, image)
	})
	return ExecResult(r), err
}

func (s *AdminService) DockerRmi(ctx context.Context, sessionID, id string) error {
	_, err := onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) (any, error) {
		return nil, e.Rmi(ctx, id)
	})
	return err
}

func (s *AdminService) DockerPrune(ctx context.Context, sessionID, what string) (ExecResult, error) {
	r, err := onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) (admin.Result, error) {
		return e.Prune(ctx, what)
	})
	return ExecResult(r), err
}

func (s *AdminService) DockerStats(ctx context.Context, sessionID string) ([]DockerStat, error) {
	l, err := onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) ([]container.Stat, error) {
		return e.Stats(ctx)
	})
	return conv(l, func(v container.Stat) DockerStat { return DockerStat(v) }), err
}

func (s *AdminService) DockerSystemDf(ctx context.Context, sessionID string) (string, error) {
	return onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) (string, error) {
		return e.SystemDf(ctx)
	})
}

// DockerLogs returns a container's last tail lines ("all" for every
// line), each with its timestamp.
func (s *AdminService) DockerLogs(ctx context.Context, sessionID, id, tail string) (string, error) {
	return onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) (string, error) {
		return e.Logs(ctx, id, tail)
	})
}

// DockerCreate runs a new container and returns its ID. A bind mount is
// confirmed in a native dialog listing the host folders first.
func (s *AdminService) DockerCreate(ctx context.Context, sessionID string, o container.CreateOptions) (string, error) {
	e, local, err := s.engine(sessionID)
	if err != nil {
		return "", err
	}
	if _, err := e.CreateArgs(o); err != nil {
		return "", err
	}
	if len(o.Binds) > 0 {
		where := "the host"
		if local {
			where = "this computer"
		}
		lines := make([]string, len(o.Binds))
		for i, m := range o.Binds {
			lines[i] = m.Source + " → " + m.Target
		}
		msg := fmt.Sprintf("The container %s can read and change these folders of %s:\n\n%s", o.Image, where, strings.Join(lines, "\n"))
		if err := s.ask(ctx, "Mount host folders?", msg); err != nil {
			return "", err
		}
	}
	ctx, cancel := execCtx(ctx)
	defer cancel()
	return e.Create(ctx, o)
}

// DockerLogin signs in to a registry. The password goes to the engine
// once, on stdin; Hostbay does not keep it.
func (s *AdminService) DockerLogin(ctx context.Context, sessionID, registry, user, password string) error {
	_, err := onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) (any, error) {
		return nil, e.Login(ctx, registry, user, password)
	})
	return err
}

// ContainerListDir and ContainerReadFile browse a running container's
// files: ls -lA of a folder, and the first 256 KiB of a file.
func (s *AdminService) ContainerListDir(ctx context.Context, sessionID, id, path string) (string, error) {
	return onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) (string, error) {
		return e.ListDir(ctx, id, path)
	})
}

func (s *AdminService) ContainerReadFile(ctx context.Context, sessionID, id, path string) (string, error) {
	return onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) (string, error) {
		return e.ReadFile(ctx, id, path)
	})
}

// ContainerTerminal opens a terminal on a container: a shell through exec
// -it, or with follow its logs as they come. owner groups it like a
// session ID, so ConnService.Disconnect(owner) closes it. On this
// computer, a shell in a privileged or bind-mounted container is confirmed
// in a native dialog first.
func (s *AdminService) ContainerTerminal(ctx context.Context, owner, sessionID, id, shell string, follow bool, cols, rows int) (string, error) {
	e, local, err := s.engine(sessionID)
	if err != nil {
		return "", err
	}
	argv, err := e.FollowArgv(id)
	if !follow {
		argv, err = e.ExecArgv(id, shell)
	}
	if err != nil {
		return "", err
	}
	if !follow && local {
		ictx, cancel := execCtx(ctx)
		x, err := e.Inspect(ictx, id)
		cancel()
		if err != nil || x.Privileged || len(x.Binds) > 0 {
			msg := "A shell in container " + id + " can reach this computer:"
			if x.Privileged {
				msg += "\n\nIt is privileged."
			}
			if len(x.Binds) > 0 {
				msg += "\n\nThese folders of this computer are mounted in it:\n" + strings.Join(x.Binds, "\n")
			}
			if err != nil {
				msg = "Hostbay could not check what container " + id + " can reach on this computer."
			}
			if err := s.ask(ctx, "Open a shell in this container?", msg); err != nil {
				return "", err
			}
		}
	}
	ctx, cancel := execCtx(ctx)
	defer cancel()
	t, err := e.Runner.PTY(ctx, argv, cols, rows)
	if err != nil {
		return "", err
	}
	return s.conns.terms.Open(owner, t), nil
}

// ComposeProjects finds compose files. On a remote host they are searched
// for; on this computer they are the files the user picked (extraPaths),
// since a search of the home folder would trip macOS's folder prompts.
func (s *AdminService) ComposeProjects(ctx context.Context, sessionID string, extraPaths []string) ([]ComposeProject, error) {
	if _, local, err := s.engine(sessionID); err != nil || local {
		var list []ComposeProject
		for _, p := range extraPaths {
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && filepath.IsAbs(p) {
				list = append(list, ComposeProject{Name: filepath.Base(filepath.Dir(p)), Path: p})
			}
		}
		return list, err
	}
	l, err := call(s, ctx, sessionID, func(ctx context.Context, c *ssh.Client) ([]admin.ComposeProject, error) {
		return admin.ComposeProjects(ctx, c, extraPaths)
	})
	return conv(l, func(v admin.ComposeProject) ComposeProject { return ComposeProject(v) }), err
}

// ComposeAction runs a Compose action. On this computer, up and restart
// are confirmed in a native dialog naming the compose file first.
func (s *AdminService) ComposeAction(ctx context.Context, sessionID, path, action string) (ExecResult, error) {
	e, local, err := s.engine(sessionID)
	if err != nil {
		return ExecResult{}, err
	}
	if local && (action == "up" || action == "restart") {
		msg := "Compose " + action + " starts the services of " + path + " on this computer. They can mount its folders and open its ports."
		if err := s.ask(ctx, "Start this Compose project?", msg); err != nil {
			return ExecResult{}, err
		}
	}
	ctx, cancel := execCtx(ctx)
	defer cancel()
	r, err := e.ComposeAction(ctx, path, action)
	return ExecResult(r), err
}

// DockerFixPermission adds the remote user to the docker group. It never
// runs on this computer: Hostbay does not change local groups or use sudo.
func (s *AdminService) DockerFixPermission(ctx context.Context, sessionID string) (ExecResult, error) {
	if _, local, err := s.engine(sessionID); err != nil || local {
		return ExecResult{}, errors.Join(err, errors.New("only a remote host's Docker permission can be fixed here"))
	}
	return callExec(s, ctx, sessionID, admin.DockerFixPermission)
}

const wslPrefix = "wsl:"

// ContainerLocalEngines lists the engines on this computer: each binary
// found, and on Windows docker, podman and nerdctl inside each WSL
// distribution, which are not probed, since that would start the
// distribution.
func (*AdminService) ContainerLocalEngines() []ContainerEngine {
	var out []ContainerEngine
	for _, k := range container.Kinds {
		if k == container.WSLC && runtime.GOOS != "windows" {
			continue
		}
		if p, err := container.Find(k); err == nil {
			out = append(out, ContainerEngine{ID: string(k), Label: string(k), Path: p})
		}
	}
	for _, d := range wslDistros() {
		for _, k := range []container.Kind{container.Docker, container.Podman, container.Nerdctl} {
			out = append(out, ContainerEngine{ID: wslPrefix + d + ":" + string(k), Label: string(k) + " (" + d + ", WSL)"})
		}
	}
	return out
}

// wslDistros names the installed WSL distributions; none off Windows.
func wslDistros() []string {
	var ds []string
	for _, sh := range terminal.ListLocalShells() {
		if d, ok := strings.CutPrefix(sh.ID, wslPrefix); ok {
			ds = append(ds, d)
		}
	}
	return ds
}

// openLocal resolves a ContainerEngine ID on this computer. An engine's
// socket must be on this computer; a tcp:// or ssh:// one is refused.
func openLocal(ctx context.Context, id string) (*engineChoice, error) {
	if rest, ok := strings.CutPrefix(id, wslPrefix); ok {
		i := strings.LastIndex(rest, ":")
		if i < 0 || !slices.Contains(wslDistros(), rest[:i]) {
			return nil, errors.New("unknown WSL distribution")
		}
		kind := container.Kind(rest[i+1:])
		if _, err := container.New(kind, "", nil); err != nil || kind == container.WSLC {
			return nil, fmt.Errorf("unknown container engine: %q", kind)
		}
		return &engineChoice{id: id, kind: kind, runner: container.WSLRunner{Distro: rest[:i]}}, nil
	}
	l, err := container.OpenLocal(ctx, container.Kind(id))
	if err != nil {
		return nil, err
	}
	return &engineChoice{id: id, kind: l.Kind, runner: l.Runner, local: &l}, nil
}

// ContainerOpenLocal opens the Containers tab's stand-in session for an
// engine on this computer (a ContainerLocalEngines ID) and returns its ID.
// ContainerClose ends it.
func (s *AdminService) ContainerOpenLocal(ctx context.Context, engineID string) (string, error) {
	c, err := openLocal(ctx, engineID)
	if err != nil {
		return "", err
	}
	id := newRequestID()
	s.mu.Lock()
	s.engines[id] = c
	s.mu.Unlock()
	return id, nil
}

func (s *AdminService) ContainerClose(sessionID string) {
	s.mu.Lock()
	c := s.engines[sessionID]
	if c != nil && c.runner != nil {
		delete(s.engines, sessionID)
	}
	s.mu.Unlock()
}

// ContainerUse switches the session's engine (a ContainerEngine ID) and,
// for nerdctl, its namespace.
func (s *AdminService) ContainerUse(ctx context.Context, sessionID, engineID, namespace string) error {
	s.mu.Lock()
	cur := s.engines[sessionID]
	s.mu.Unlock()
	var next *engineChoice
	if cur != nil && cur.runner != nil {
		next = cur
		if engineID != cur.id {
			c, err := openLocal(ctx, engineID)
			if err != nil {
				return err
			}
			next = c
		}
	} else {
		if s.mgr.Lease(sessionID) == nil {
			return errors.New("the session has ended; connect again")
		}
		next = &engineChoice{id: engineID, kind: container.Kind(engineID)}
	}
	if _, err := container.New(next.kind, namespace, nil); err != nil {
		return err
	}
	c := *next
	c.ns = namespace
	s.mu.Lock()
	s.engines[sessionID] = &c
	s.mu.Unlock()
	return nil
}

// ContainerInfo describes the session's engine and the others it can use:
// on a remote host the engines its dashboard probe finds.
func (s *AdminService) ContainerInfo(ctx context.Context, sessionID string) (ContainerInfo, error) {
	e, local, err := s.engine(sessionID)
	if err != nil {
		return ContainerInfo{}, err
	}
	info := ContainerInfo{Engine: string(e.Kind), Kind: string(e.Kind), Namespace: e.Namespace, Local: local, Caps: e.Caps()}
	if local {
		s.mu.Lock()
		c := s.engines[sessionID]
		s.mu.Unlock()
		info.Engine = c.id
		if c.local != nil {
			info.Path, info.Context, info.Endpoint = c.local.Path, c.local.Context, c.local.Endpoint
		}
		info.Engines = s.ContainerLocalEngines()
		return info, nil
	}
	d, err := s.DashboardInfo(ctx, sessionID)
	if err != nil {
		return ContainerInfo{}, err
	}
	for _, k := range []struct {
		kind container.Kind
		ok   bool
	}{{container.Docker, d.DockerInstalled}, {container.Podman, d.PodmanInstalled}, {container.Nerdctl, d.NerdctlInstalled}} {
		if k.ok || k.kind == e.Kind {
			info.Engines = append(info.Engines, ContainerEngine{ID: string(k.kind), Label: string(k.kind)})
		}
	}
	return info, nil
}

// ContainerNamespaces lists nerdctl's namespaces for the namespace picker.
func (s *AdminService) ContainerNamespaces(ctx context.Context, sessionID string) ([]string, error) {
	return onEngine(s, ctx, sessionID, func(ctx context.Context, e *container.Engine, _ bool) ([]string, error) {
		return e.Namespaces(ctx)
	})
}

func (s *AdminService) NginxTest(ctx context.Context, sessionID string) (ExecResult, error) {
	return callExec(s, ctx, sessionID, admin.NginxTest)
}

func (s *AdminService) NginxReload(ctx context.Context, sessionID string) (ExecResult, error) {
	return callExec(s, ctx, sessionID, admin.NginxReload)
}

func (s *AdminService) SystemdList(ctx context.Context, sessionID string) ([]SystemdUnit, error) {
	l, err := call(s, ctx, sessionID, admin.SystemdList)
	return conv(l, func(v admin.SystemdUnit) SystemdUnit { return SystemdUnit(v) }), err
}

func (s *AdminService) SystemdAction(ctx context.Context, sessionID, unit, action string) (ExecResult, error) {
	return callExec(s, ctx, sessionID, func(ctx context.Context, c *ssh.Client) (admin.Result, error) {
		return admin.SystemdAction(ctx, c, unit, action)
	})
}

func (s *AdminService) DashboardInfo(ctx context.Context, sessionID string) (DashboardInfo, error) {
	d, err := call(s, ctx, sessionID, admin.Dashboard)
	return DashboardInfo(d), err
}

func (s *AdminService) IisSites(ctx context.Context, sessionID string) ([]IisSite, error) {
	l, err := call(s, ctx, sessionID, admin.IisSites)
	return conv(l, func(v admin.IisSite) IisSite { return IisSite(v) }), err
}

func (s *AdminService) IisPools(ctx context.Context, sessionID string) ([]IisPool, error) {
	l, err := call(s, ctx, sessionID, admin.IisPools)
	return conv(l, func(v admin.IisPool) IisPool { return IisPool(v) }), err
}

func (s *AdminService) IisSiteAction(ctx context.Context, sessionID, name, action string) (ExecResult, error) {
	return callExec(s, ctx, sessionID, func(ctx context.Context, c *ssh.Client) (admin.Result, error) {
		return admin.IisSiteAction(ctx, c, name, action)
	})
}

func (s *AdminService) IisPoolAction(ctx context.Context, sessionID, name, action string) (ExecResult, error) {
	return callExec(s, ctx, sessionID, func(ctx context.Context, c *ssh.Client) (admin.Result, error) {
		return admin.IisPoolAction(ctx, c, name, action)
	})
}

func (s *AdminService) WindowsDashboard(ctx context.Context, sessionID string) (WinDashboard, error) {
	d, err := call(s, ctx, sessionID, admin.WindowsDashboard)
	return WinDashboard(d), err
}

// WolWake broadcasts the magic packet from this computer.
func (*AdminService) WolWake(mac string) error { return wol.Wake(mac) }

// WolWakeVia sends it from a connected host on the target's LAN.
func (s *AdminService) WolWakeVia(ctx context.Context, relaySessionID, mac string) error {
	_, err := call(s, ctx, relaySessionID, func(ctx context.Context, c *ssh.Client) (any, error) {
		return nil, wol.WakeVia(ctx, c, mac)
	})
	return err
}
