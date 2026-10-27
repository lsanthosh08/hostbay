package app

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/secrets"
	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx"
	"hostbay/internal/tunnel"
)

type tunnelEnv struct {
	svc    *TunnelService
	srv    *sftptest.Server
	hostID string

	mu     sync.Mutex
	events []TunnelState
}

// newTunnelEnv saves a password host for an in-process SSH server whose key
// is already trusted, and returns a tunnel service recording its events.
func newTunnelEnv(t *testing.T) *tunnelEnv {
	t.Helper()
	hs, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	srv := sftptest.New(t, "pw")
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conns := NewConnService(NewHostKeyTrust(sshx.NewKnownHostAppender(kh)), creds, sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh}))
	conns.emit = func(string, any) {}
	host, portStr, _ := net.SplitHostPort(srv.Addr)
	port, _ := strconv.Atoi(portStr)
	pw, auth := "pw", "password"
	h, err := hs.SaveHost(Host{Alias: "srv", Hostname: host, Port: port, User: "u", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	e := &tunnelEnv{svc: NewTunnelService(hs.st, conns), srv: srv, hostID: h.ID}
	e.svc.emit = func(name string, data any) {
		if name != TunnelStateEvent {
			t.Errorf("event %q", name)
		}
		e.mu.Lock()
		e.events = append(e.events, data.(TunnelState))
		e.mu.Unlock()
	}
	t.Cleanup(func() { e.svc.ServiceShutdown() })
	return e
}

func (e *tunnelEnv) lastEvent(id string) TunnelState {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := len(e.events) - 1; i >= 0; i-- {
		if e.events[i].ID == id {
			return e.events[i]
		}
	}
	return TunnelState{}
}

func (e *tunnelEnv) save(t *testing.T, tn Tunnel) Tunnel {
	t.Helper()
	tn.HostID = e.hostID
	out, err := e.svc.SaveTunnel(tn)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *tunnelEnv) session(id string) string {
	e.svc.mu.Lock()
	defer e.svc.mu.Unlock()
	if r := e.svc.runs[id]; r != nil {
		return r.session
	}
	return ""
}

func echoAddr(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func echoesThrough(t *testing.T, addr string) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(c, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("read %q, %v", buf, err)
	}
}

func TestTunnelStartListensAndStopReleasesTheLease(t *testing.T) {
	e := newTunnelEnv(t)
	host, port := echoAddr(t)
	tn := e.save(t, Tunnel{Name: "echo", Kind: "local", TargetHost: host, TargetPort: port})

	st, err := e.svc.StartTunnel(context.Background(), tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != TunnelListening || st.Addr == "" || e.lastEvent(tn.ID) != st {
		t.Fatalf("state = %+v, last event %+v", st, e.lastEvent(tn.ID))
	}
	if n := e.svc.RunningTunnels(); n != 1 {
		t.Fatalf("RunningTunnels = %d", n)
	}
	echoesThrough(t, st.Addr)
	sid := e.session(tn.ID)
	if e.svc.conns.mgr.Lease(sid) == nil {
		t.Fatal("running tunnel holds no lease")
	}
	if again, _ := e.svc.StartTunnel(context.Background(), tn.ID); again != st {
		t.Fatalf("second start = %+v, want the running state", again)
	}

	if err := e.svc.StopTunnel(tn.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.lastEvent(tn.ID); got.Status != TunnelStopped {
		t.Fatalf("after stop: %+v", got)
	}
	if e.svc.conns.mgr.Lease(sid) != nil {
		t.Fatal("stopped tunnel kept its lease")
	}
	if c, err := net.DialTimeout("tcp", st.Addr, time.Second); err == nil {
		c.Close()
		t.Fatal("still listening after stop")
	}
	if n := e.svc.RunningTunnels(); n != 0 {
		t.Fatalf("RunningTunnels after stop = %d", n)
	}
}

// Covers AE12.
func TestTunnelOnBusyPortShowsFailedWithReason(t *testing.T) {
	e := newTunnelEnv(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	tn := e.save(t, Tunnel{Name: "busy", Kind: "dynamic", BindPort: busy.Addr().(*net.TCPAddr).Port})

	st, err := e.svc.StartTunnel(context.Background(), tn.ID)
	if err == nil {
		t.Fatal("started on a port in use")
	}
	if st.Status != TunnelFailed || !strings.Contains(st.Error, busy.Addr().String()) || e.lastEvent(tn.ID) != st {
		t.Fatalf("state = %+v", st)
	}
	if n := e.svc.RunningTunnels(); n != 0 {
		t.Fatalf("a failed tunnel counts as running: %d", n)
	}
}

// Dropping the connection marks the tunnel dropped, frees its port and its
// lease, and does not restart it; starting again reconnects.
func TestTunnelDropShowsDroppedAndFreesPort(t *testing.T) {
	e := newTunnelEnv(t)
	host, port := echoAddr(t)
	tn := e.save(t, Tunnel{Name: "echo", Kind: "local", TargetHost: host, TargetPort: port})
	st, err := e.svc.StartTunnel(context.Background(), tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	sid := e.session(tn.ID)

	e.srv.DropAll()
	waitUntil(t, "dropped state", func() bool { return e.lastEvent(tn.ID).Status == TunnelDropped })
	if got := e.lastEvent(tn.ID); got.Error == "" {
		t.Fatalf("dropped without a reason: %+v", got)
	}
	ln, err := net.Listen("tcp", st.Addr)
	if err != nil {
		t.Fatalf("port not freed: %v", err)
	}
	ln.Close()
	if e.svc.conns.mgr.Lease(sid) != nil {
		t.Fatal("dropped tunnel kept its lease")
	}
	time.Sleep(50 * time.Millisecond)
	if got := e.lastEvent(tn.ID); got.Status != TunnelDropped {
		t.Fatalf("tunnel restarted by itself: %+v", got)
	}

	st, err = e.svc.StartTunnel(context.Background(), tn.ID)
	if err != nil || st.Status != TunnelListening {
		t.Fatalf("restart after drop: %+v, %v", st, err)
	}
	echoesThrough(t, st.Addr)
}

func TestSaveTunnelValidates(t *testing.T) {
	e := newTunnelEnv(t)
	_, err := e.svc.SaveTunnel(Tunnel{HostID: e.hostID, Kind: "dynamic", BindHost: "0.0.0.0", BindPort: 1080})
	if !errors.Is(err, tunnel.ErrExternalBind) {
		t.Fatalf("non-loopback without consent: %v", err)
	}
	if _, err := e.svc.SaveTunnel(Tunnel{HostID: e.hostID, Kind: "dynamic", BindHost: "0.0.0.0", BindPort: 1080, AllowExternal: true}); err != nil {
		t.Fatalf("non-loopback with consent: %v", err)
	}
	if _, err := e.svc.SaveTunnel(Tunnel{HostID: "nope", Kind: "dynamic", BindPort: 1080}); err == nil {
		t.Fatal("tunnel saved for a missing host")
	}
	list, err := e.svc.ListTunnels()
	if err != nil || len(list) != 1 || !list[0].AllowExternal || list[0].ID == "" {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestDeleteTunnelStopsItFirst(t *testing.T) {
	e := newTunnelEnv(t)
	tn := e.save(t, Tunnel{Name: "socks", Kind: "dynamic"})
	st, err := e.svc.StartTunnel(context.Background(), tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteTunnel(tn.ID); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", st.Addr, time.Second); err == nil {
		c.Close()
		t.Fatal("deleted tunnel still listening")
	}
	if list, _ := e.svc.ListTunnels(); len(list) != 0 {
		t.Fatalf("list after delete = %+v", list)
	}
	if states := e.svc.TunnelStates(); len(states) != 0 {
		t.Fatalf("states after delete = %+v", states)
	}
}
