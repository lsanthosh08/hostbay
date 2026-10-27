package app

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"

	"hostbay/internal/monitor"
	"hostbay/internal/sftp/sftptest"
)

type monitorEnv struct {
	svc   *MonitorService
	conns *ConnService
	srv   *sftptest.Server
	host  string
	tick  chan time.Time

	mu     sync.Mutex
	events map[string][]monitor.Event // event name -> events
}

// newMonitorEnv saves a host for an in-process SSH server whose uname says
// Linux, and returns a monitor service whose ticks the test sends.
func newMonitorEnv(t *testing.T) *monitorEnv {
	t.Helper()
	srv := sftptest.New(t, "pw")
	srv.Command(t, "uname", "#!/bin/sh\necho Linux")
	conns, hs, _ := connWithKnownHosts(t, srv.KnownHostsLine())
	conns.emit = func(string, any) {}
	host, portStr, _ := net.SplitHostPort(srv.Addr)
	port, _ := strconv.Atoi(portStr)
	pw, auth := "pw", "password"
	h, err := hs.SaveHost(Host{Alias: "srv", Hostname: host, Port: port, User: "u", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	return newMonitorEnvOn(t, conns, srv, h.ID)
}

func newMonitorEnvOn(t *testing.T, conns *ConnService, srv *sftptest.Server, hostID string) *monitorEnv {
	e := &monitorEnv{conns: conns, srv: srv, host: hostID, tick: make(chan time.Time), events: map[string][]monitor.Event{}}
	e.svc = NewMonitorService(conns)
	e.svc.tick = e.tick
	e.svc.emit = func(name string, data any) {
		e.mu.Lock()
		e.events[name] = append(e.events[name], data.(monitor.Event))
		e.mu.Unlock()
	}
	t.Cleanup(func() { e.svc.ServiceShutdown() })
	return e
}

func (e *monitorEnv) session(t *testing.T) string {
	t.Helper()
	id, err := e.conns.Connect(context.Background(), e.host)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *monitorEnv) start(t *testing.T, session string) MonitorStart {
	t.Helper()
	m, err := e.svc.Start(context.Background(), session, e.host)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// scripts counts the sampling execs the server received.
func (e *monitorEnv) scripts() int {
	n := 0
	for _, c := range e.srv.Commands() {
		if c == monitor.Script {
			n++
		}
	}
	return n
}

func (e *monitorEnv) waitScripts(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for e.scripts() < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.scripts(); got != n {
		t.Fatalf("sampling execs = %d, want %d", got, n)
	}
}

// waitSamples waits until monitor id has had n sample events, so no
// sample is in flight when the test ticks next.
func (e *monitorEnv) waitSamples(t *testing.T, id string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := 0
		e.mu.Lock()
		for _, ev := range e.events[MonitorEvent(id)] {
			if ev.Sample != nil {
				got++
			}
		}
		e.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fewer than %d samples for %s", n, id)
}

// tickOrStopped sends a tick unless no sampler is left to take it.
func (e *monitorEnv) tickOrStopped() {
	select {
	case e.tick <- time.Now():
	case <-time.After(100 * time.Millisecond):
	}
}

func (e *monitorEnv) settle(t *testing.T, n int) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	if got := e.scripts(); got != n {
		t.Fatalf("sampling execs = %d, want %d", got, n)
	}
}

func TestTwoMonitorTabsOnOneHostShareOneExecPerTick(t *testing.T) {
	e := newMonitorEnv(t)
	a := e.start(t, e.session(t))
	e.waitSamples(t, a.ID, 1)
	b := e.start(t, e.session(t))
	if len(b.History) != 1 || b.State.State != monitor.StateLive {
		t.Fatalf("second tab got %+v", b)
	}
	e.waitScripts(t, 1)
	e.tick <- time.Now()
	e.waitScripts(t, 2)
	e.waitSamples(t, b.ID, 1)
	e.settle(t, 2)
}

func TestMonitorStopsWithItsLastTab(t *testing.T) {
	e := newMonitorEnv(t)
	sess := e.session(t)
	a, b := e.start(t, sess), e.start(t, sess)
	e.waitSamples(t, b.ID, 1)
	e.svc.Stop(a.ID)
	e.tick <- time.Now()
	e.waitScripts(t, 2)
	lease := e.svc.hosts[e.host].lease
	e.svc.Stop(b.ID)
	e.tickOrStopped()
	e.settle(t, 2)
	if e.conns.mgr.Lease(lease) != nil {
		t.Fatal("the monitor still holds its lease")
	}
}

func TestHiddenMonitorTabsPauseSampling(t *testing.T) {
	e := newMonitorEnv(t)
	sess := e.session(t)
	a, b := e.start(t, sess), e.start(t, sess)
	e.waitSamples(t, b.ID, 1)
	e.svc.SetVisible(a.ID, false)
	e.tick <- time.Now()
	e.waitScripts(t, 2) // b still shows
	e.waitSamples(t, b.ID, 2)
	e.svc.SetVisible(b.ID, false)
	e.tick <- time.Now()
	e.tick <- time.Now()
	e.settle(t, 2)
	e.svc.SetVisible(a.ID, true) // samples at once
	e.waitScripts(t, 3)
}

func TestDisconnectStopsTheSessionsMonitor(t *testing.T) {
	e := newMonitorEnv(t)
	sess := e.session(t)
	e.start(t, sess)
	e.waitScripts(t, 1)
	if err := e.conns.Disconnect(sess); err != nil {
		t.Fatal(err)
	}
	e.tickOrStopped()
	e.settle(t, 1)
	if len(e.svc.hosts) != 0 {
		t.Fatal("the sampler outlived the session")
	}
}

// No host field reaches the remote command: with an alias that is a shell
// substitution and a hostname holding backticks (reached through a jump
// host that resolves it), the host receives Script byte for byte.
func TestMonitorCommandIsTheConstantScript(t *testing.T) {
	target := sftptest.New(t, "pw")
	target.Command(t, "uname", "#!/bin/sh\necho Linux")
	jump := sftptest.New(t, "jump-pw")
	name := "web`id`"
	tport, _ := strconv.Atoi(target.Addr[len("127.0.0.1:"):])
	jump.Resolve(name, target.Addr)
	conns, hs, _ := connWithKnownHosts(t, jump.KnownHostsLine(),
		knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort(name, strconv.Itoa(tport)))}, target.HostKey))
	conns.emit = func(string, any) {}
	jhost, jport, _ := net.SplitHostPort(jump.Addr)
	jp, _ := strconv.Atoi(jport)
	auth, jpw, pw := "password", "jump-pw", "pw"
	j, err := hs.SaveHost(Host{Alias: "bastion", Hostname: jhost, Port: jp, User: "u", AuthType: &auth, Password: &jpw})
	if err != nil {
		t.Fatal(err)
	}
	h, err := hs.SaveHost(Host{Alias: "$(id)", Hostname: name, Port: tport, User: "u", AuthType: &auth, Password: &pw, JumpHostID: &j.ID})
	if err != nil {
		t.Fatal(err)
	}
	e := newMonitorEnvOn(t, conns, target, h.ID)
	m := e.start(t, e.session(t))
	e.waitSamples(t, m.ID, 1)
	sampled := 0
	for _, c := range target.Commands() {
		if c == detectMACCommand {
			continue
		}
		if c != monitor.Script {
			t.Fatalf("the host received %q", c)
		}
		sampled++
	}
	if sampled == 0 {
		t.Fatal("no sample ran")
	}
}

func TestMonitorStartNeedsALiveSession(t *testing.T) {
	e := newMonitorEnv(t)
	if _, err := e.svc.Start(context.Background(), "gone", e.host); err == nil {
		t.Fatal("started on a session that does not exist")
	}
	if e.scripts() != 0 {
		t.Fatal("sampled anyway")
	}
}
