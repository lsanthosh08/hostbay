package monitor

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeHost is an SSH server whose exec requests all run handle, counting
// each one, as internal/admin/exec_test.go's server does.
type fakeHost struct {
	client *ssh.Client
	execs  atomic.Int32
	handle func(cmd string, w io.Writer)
}

func newFakeHost(t *testing.T, handle func(cmd string, w io.Writer)) *fakeHost {
	t.Helper()
	h := &fakeHost{handle: handle}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hk, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(hk)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					go h.serve(nch)
				}
			}()
		}
	}()
	c, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{User: "u", HostKeyCallback: ssh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	h.client = c
	return h
}

func (h *fakeHost) serve(nch ssh.NewChannel) {
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		var p struct{ Cmd string }
		ssh.Unmarshal(req.Payload, &p)
		req.Reply(true, nil)
		h.execs.Add(1)
		go ssh.DiscardRequests(reqs)
		h.handle(p.Cmd, ch)
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		return
	}
}

// events records what a sampler emits.
type events struct {
	mu  sync.Mutex
	all []Event
}

func (e *events) add(ev Event) {
	e.mu.Lock()
	e.all = append(e.all, ev)
	e.mu.Unlock()
}

// wait returns the first event after the first skip events that matches.
func (e *events) wait(t *testing.T, match func(Event) bool) Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		for _, ev := range e.all {
			if match(ev) {
				e.mu.Unlock()
				return ev
			}
		}
		e.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no matching event in %+v", e.all)
	return Event{}
}

func waitExecs(t *testing.T, h *fakeHost, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.execs.Load() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.execs.Load(); got != n {
		t.Fatalf("execs = %d, want %d", got, n)
	}
}

// settle lets anything the sampler is about to send arrive, then checks
// the exec count did not move.
func settle(t *testing.T, h *fakeHost, n int32) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	if got := h.execs.Load(); got != n {
		t.Fatalf("execs = %d, want %d", got, n)
	}
}

const linuxOut = "OS::Linux\n:STAT::\ncpu 100 0 100 800 0 0 0 0\n"

func newTestSampler(t *testing.T, h *fakeHost, client func() *ssh.Client) (*Sampler, chan time.Time, *events) {
	t.Helper()
	tick := make(chan time.Time)
	ev := &events{}
	if client == nil {
		client = func() *ssh.Client { return h.client }
	}
	s := New(Config{Client: client, Emit: ev.add, Tick: tick})
	t.Cleanup(s.Stop)
	return s, tick, ev
}

func TestSamplerRunsTheConstantScriptAndEmitsASample(t *testing.T) {
	var got atomic.Value
	h := newFakeHost(t, func(cmd string, w io.Writer) { got.Store(cmd); io.WriteString(w, linuxOut) })
	s, _, ev := newTestSampler(t, h, nil)
	e := ev.wait(t, func(e Event) bool { return e.Sample != nil })
	if e.State != StateLive || got.Load() != Script {
		t.Fatalf("event %+v, command %q", e, got.Load())
	}
	if hist := s.History(); len(hist) != 1 {
		t.Fatalf("history %d", len(hist))
	}
}

func TestSlowTickSkipsInsteadOfQueueing(t *testing.T) {
	release := make(chan struct{})
	var first atomic.Bool
	h := newFakeHost(t, func(_ string, w io.Writer) {
		if first.CompareAndSwap(false, true) {
			<-release
		}
		io.WriteString(w, linuxOut)
	})
	_, tick, ev := newTestSampler(t, h, nil)
	waitExecs(t, h, 1) // the first sample runs at start and hangs
	tick <- time.Now()
	tick <- time.Now()
	close(release)
	ev.wait(t, func(e Event) bool { return e.Sample != nil })
	settle(t, h, 1)
	tick <- time.Now()
	waitExecs(t, h, 2)
}

func TestOutputIsCutAtTheCapAndMarkedPartial(t *testing.T) {
	h := newFakeHost(t, func(_ string, w io.Writer) {
		io.WriteString(w, linuxOut+":PS::\nPID COMMAND\n")
		line := "1 " + strings.Repeat("x", 1022) + "\n"
		for range 5 * 1024 {
			if _, err := io.WriteString(w, line); err != nil {
				return
			}
		}
	})
	out, partial, err := run(t.Context(), h.client, MaxOutput)
	if err != nil || !partial || len(out) != MaxOutput {
		t.Fatalf("len %d partial %v err %v", len(out), partial, err)
	}
	_, _, ev := newTestSampler(t, h, nil)
	e := ev.wait(t, func(e Event) bool { return e.Sample != nil })
	if !e.Sample.Partial || e.State != StateLive {
		t.Fatalf("event %+v", e)
	}
}

func TestNonLinuxShowsUnsupportedAndStops(t *testing.T) {
	h := newFakeHost(t, func(_ string, w io.Writer) { io.WriteString(w, "OS::Darwin\n") })
	_, tick, ev := newTestSampler(t, h, nil)
	e := ev.wait(t, func(e Event) bool { return e.State == StateUnsupported })
	if e.OS != "Darwin" {
		t.Fatalf("event %+v", e)
	}
	select {
	case tick <- time.Now():
	case <-time.After(100 * time.Millisecond):
	}
	settle(t, h, 1)
}

func TestPausedSamplerDoesNotExecAndResumesAtOnce(t *testing.T) {
	h := newFakeHost(t, func(_ string, w io.Writer) { io.WriteString(w, linuxOut) })
	s, tick, _ := newTestSampler(t, h, nil)
	waitExecs(t, h, 1)
	s.SetPaused(true)
	tick <- time.Now()
	tick <- time.Now()
	settle(t, h, 1)
	s.SetPaused(false)
	waitExecs(t, h, 2)
}

func TestStoppedSamplerDoesNotExec(t *testing.T) {
	h := newFakeHost(t, func(_ string, w io.Writer) { io.WriteString(w, linuxOut) })
	s, tick, _ := newTestSampler(t, h, nil)
	waitExecs(t, h, 1)
	s.Stop()
	select {
	case tick <- time.Now():
		t.Fatal("a stopped sampler took a tick")
	case <-time.After(50 * time.Millisecond):
	}
	settle(t, h, 1)
}

func TestDroppedConnectionShowsDisconnectedWithoutExec(t *testing.T) {
	var up atomic.Bool
	h := newFakeHost(t, func(_ string, w io.Writer) { io.WriteString(w, linuxOut) })
	_, tick, ev := newTestSampler(t, h, func() *ssh.Client {
		if up.Load() {
			return h.client
		}
		return nil
	})
	ev.wait(t, func(e Event) bool { return e.State == StateDisconnected })
	tick <- time.Now()
	settle(t, h, 0)
	up.Store(true) // the session reconnected
	tick <- time.Now()
	waitExecs(t, h, 1)
	ev.wait(t, func(e Event) bool { return e.State == StateLive })
}

func TestFailuresBackOff(t *testing.T) {
	h := newFakeHost(t, func(_ string, w io.Writer) {}) // no output at all
	_, tick, ev := newTestSampler(t, h, nil)
	ev.wait(t, func(e Event) bool { return e.State == StateError })
	tick <- time.Now() // inside the backoff
	settle(t, h, 1)
}
