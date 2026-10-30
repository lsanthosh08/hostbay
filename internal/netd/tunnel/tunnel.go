// Package tunnel runs hostbay-netd's system-wide tunnels (U12, KTD8):
// OpenVPN from a configuration the helper writes itself, OpenConnect on a
// Wintun adapter on Windows, and WireGuard on a real TUN. Only
// cmd/hostbay-netd imports it, which keeps WireGuard out of Hostbay.
//
// The helper starts only the VPN client copies install placed in its
// root-owned directory, checking them before each start
// (netd.CheckClient), with no shell. Each tunnel's files live in the run
// folder under a handle Hostbay chose, never a profile's name or ID, and
// go when it stops; the folder is emptied whenever the helper starts.
package tunnel

import (
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"golang.zx2c4.com/wireguard/tun"

	"hostbay/internal/netd"
)

// stopWait is how long a stopping client gets to exit by itself, removing
// what it set up, before it is killed.
var stopWait = 5 * time.Second

// process is a started client; tests use a fake.
type process interface {
	Wait() error
	Stop() error // ask it to exit: SIGTERM, or a kill on Windows
	Kill() error
}

type cmdProcess struct{ *exec.Cmd }

func (p cmdProcess) Kill() error { return p.Process.Kill() }

func (p cmdProcess) Stop() error {
	if runtime.GOOS == "windows" {
		return p.Process.Kill() // Hostbay asked OpenVPN to exit through its management interface first
	}
	return p.Process.Signal(syscall.SIGTERM)
}

// Manager is the helper's netd.Tunnels.
type Manager struct {
	env netd.TunnelEnv
	run string // the run folder
	// childEnv is the whole environment the clients start with (clientEnv).
	childEnv []string

	// The system's ways to start a client with no shell, create a TUN and
	// address it, and create a Wintun adapter; tests replace them.
	start     func(path string, args []string, stdin io.Reader, stdout io.Writer) (process, error)
	createTUN func(name string, mtu int) (tun.Device, error)
	setAddrs  func(dev tun.Device, name string, addrs []netip.Prefix) error
	adapter   func(name string) (io.Closer, error)

	mu      sync.Mutex
	tunnels map[string]*tunnel
}

// tunnel is one running tunnel.
type tunnel struct {
	stop func()        // asks it to end
	done chan struct{} // closed once it has ended and its files are gone
}

// New is netd.NewTunnels.
func New(env netd.TunnelEnv) netd.Tunnels {
	m := &Manager{env: env, run: filepath.Join(env.Dir, "run"), createTUN: tun.CreateTUN, setAddrs: setAddrs,
		adapter: createAdapter, tunnels: map[string]*tunnel{}}
	m.start = func(path string, args []string, stdin io.Reader, stdout io.Writer) (process, error) {
		return startProcess(path, args, m.childEnv, stdin, stdout)
	}
	os.RemoveAll(m.run) // a previous run's sockets and configurations
	os.MkdirAll(m.run, 0o755)
	os.Chmod(m.run, 0o755)
	restrictDir(m.run)
	conf := filepath.Join(m.run, "openssl.cnf")
	writePrivate(conf, "") // left missing, OpenSSL still skips its compiled-in one
	m.childEnv = clientEnv(conf, filepath.Dir(netd.ClientPath(env.Dir, "openvpn")))
	return m
}

// clientEnv is the whole environment a VPN client starts with, inheriting
// nothing. OpenSSL in a client otherwise reads the configuration, providers
// and engines its build names, which for Homebrew's lie in a folder the user
// can write, and would load them into a root process. conf is an empty
// configuration and libs a folder only root can write.
func clientEnv(conf, libs string) []string {
	env := []string{"OPENSSL_CONF=" + conf, "OPENSSL_MODULES=" + libs, "OPENSSL_ENGINES=" + libs}
	if runtime.GOOS != "windows" {
		return append(env, "PATH=/usr/bin:/bin:/usr/sbin:/sbin")
	}
	root := os.Getenv("SystemRoot")
	env = append(env, "PATH="+root+`\System32;`+root) // the machine PATH may hold folders users can write
	for _, k := range []string{"SystemRoot", "SystemDrive", "windir", "ComSpec", "PATHEXT", "TEMP", "TMP"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func startProcess(path string, args, env []string, stdin io.Reader, stdout io.Writer) (process, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = append([]string{}, env...) // never nil, which would inherit the helper's
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stdout
	cmd.Dir = filepath.Dir(path)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmdProcess{cmd}, nil
}

// Start starts the tunnel m asks for.
func (m *Manager) Start(req netd.Msg, event func(netd.Msg)) (netd.Msg, *netd.Error) {
	m.mu.Lock()
	_, busy := m.tunnels[req.Tunnel]
	m.mu.Unlock()
	if busy {
		return netd.Msg{}, &netd.Error{Code: netd.CodeRefused, Reason: "That tunnel is already running."}
	}
	ended := func(reason string) {
		event(netd.Msg{Tunnel: req.Tunnel, State: netd.TunnelEnded, Reason: reason})
	}
	up := func(iface string, addrs []string) {
		event(netd.Msg{Tunnel: req.Tunnel, State: netd.TunnelUp, Iface: iface, Address: addrs})
	}
	switch req.T {
	case netd.OpenVPNStart:
		return m.openVPN(req.Tunnel, req.OpenVPN, ended)
	case netd.OpenConnectStart:
		return m.openConnect(req.Tunnel, req.OpenConnect, up, ended)
	default:
		return m.wireGuard(req.Tunnel, req.WireGuard, up, ended)
	}
}

// add records a running tunnel; its goroutine calls the returned func once
// it has ended.
func (m *Manager) add(h string, stop func()) func() {
	t := &tunnel{stop: stop, done: make(chan struct{})}
	m.mu.Lock()
	m.tunnels[h] = t
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.tunnels, h)
		m.mu.Unlock()
		m.cleanup(h)
		close(t.done)
	}
}

// cleanup removes a tunnel's files.
func (m *Manager) cleanup(h string) {
	for _, ext := range []string{".conf", ".sock", ".pw"} {
		os.Remove(filepath.Join(m.run, h+ext))
	}
}

// Stop ends tunnel h and waits until it has.
func (m *Manager) Stop(h string) *netd.Error {
	m.mu.Lock()
	t := m.tunnels[h]
	m.mu.Unlock()
	if t != nil {
		t.stop()
		<-t.done
	}
	return nil
}

// StopAll ends every tunnel and waits until they have.
func (m *Manager) StopAll() {
	m.mu.Lock()
	var ts []*tunnel
	for _, t := range m.tunnels {
		ts = append(ts, t)
	}
	m.mu.Unlock()
	for _, t := range ts {
		t.stop()
	}
	for _, t := range ts {
		<-t.done
	}
}

// supervise waits for p to exit, killing it if stop was asked and it
// lingers, and returns stop.
func supervise(p process, exited chan<- error) func() {
	done := make(chan struct{})
	go func() {
		err := p.Wait()
		close(done)
		exited <- err
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.Stop()
			select {
			case <-done:
			case <-time.After(stopWait):
				p.Kill()
			}
		})
	}
}

func refused(reason string) *netd.Error { return &netd.Error{Code: netd.CodeRefused, Reason: reason} }
func failed(reason string) *netd.Error  { return &netd.Error{Code: netd.CodeFailed, Reason: reason} }
