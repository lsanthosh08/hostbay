package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/vpn/proto"
)

const (
	testKey  = "FAKEPRIVATEKEYbase64AAAAAAAAAAAAAAAAAAAAAAA="
	testUser = "hb-run-user"
	testPass = "Socks-Secret-42!"
)

// fake is an engine that connects at once and dials the host's network.
type fake struct {
	stopped chan struct{}
}

func (f *fake) Run(ctx context.Context, report func(proto.Msg)) {
	report(proto.Msg{T: proto.State, State: proto.Connecting})
	report(proto.Msg{T: proto.State, State: proto.Connected, Address: []string{"10.0.0.1/32"}, Routes: []string{"10.0.0.0/24"}})
	<-ctx.Done()
	close(f.stopped)
}

func (f *fake) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, address)
}

func useFake(t *testing.T) *fake {
	f := &fake{stopped: make(chan struct{})}
	engines["fake"] = func(config string, secrets map[string]string) (engine, error) {
		if secrets["privateKey"] != testKey {
			t.Errorf("engine got secrets %v", secrets)
		}
		return f, nil
	}
	t.Cleanup(func() { delete(engines, "fake") })
	return f
}

// helper runs run over pipes and records everything it writes.
type helper struct {
	stdin  *io.PipeWriter
	lines  *bufio.Scanner
	all    *bytes.Buffer
	mu     *sync.Mutex
	result chan error
}

func startHelper(t *testing.T) *helper {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &helper{stdin: inW, all: &bytes.Buffer{}, mu: &sync.Mutex{}, result: make(chan error, 1)}
	h.lines = bufio.NewScanner(io.TeeReader(outR, writerFunc(func(b []byte) (int, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.all.Write(b)
	})))
	go func() {
		err := run(inR, outW)
		outW.Close()
		h.result <- err
	}()
	t.Cleanup(func() { inW.Close() })
	return h
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func (h *helper) read(t *testing.T) proto.Msg {
	t.Helper()
	if !h.lines.Scan() {
		t.Fatalf("helper output ended: %v", h.lines.Err())
	}
	var m proto.Msg
	if err := json.Unmarshal(h.lines.Bytes(), &m); err != nil {
		t.Fatalf("bad line %q: %v", h.lines.Bytes(), err)
	}
	return m
}

func (h *helper) send(t *testing.T, m proto.Msg) {
	t.Helper()
	b, _ := json.Marshal(m)
	if _, err := h.stdin.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (h *helper) wait(t *testing.T) error {
	t.Helper()
	go func() {
		for h.lines.Scan() { // drain, so run is never blocked writing
		}
	}()
	select {
	case err := <-h.result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return")
		return nil
	}
}

func start(typ, user, pass string) proto.Msg {
	return proto.Msg{T: proto.Start, Type: typ, Config: "[Interface]",
		Secrets: map[string]string{"privateKey": testKey}, SocksUser: user, SocksPass: pass}
}

func TestRunServesUntilStdinCloses(t *testing.T) {
	f := useFake(t)
	h := startHelper(t)

	if m := h.read(t); m.T != proto.Hello || m.Version != proto.Version {
		t.Fatalf("first message %+v, want hello %d", m, proto.Version)
	}
	h.send(t, start("fake", testUser, testPass))
	if m := h.read(t); m.State != proto.Connecting {
		t.Fatalf("got %+v, want connecting", m)
	}
	m := h.read(t)
	if m.State != proto.Connected || m.SocksPort == 0 || len(m.Routes) != 1 {
		t.Fatalf("got %+v, want connected with a SOCKS port", m)
	}
	socksAddr := "127.0.0.1:" + strconv.Itoa(m.SocksPort)

	// The SOCKS endpoint takes the login from start.
	c, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{5, 1, 2})
	c.Write(append(append(append([]byte{1, byte(len(testUser))}, testUser...), byte(len(testPass))), testPass...))
	var rep [4]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil || rep[3] != 0 {
		t.Fatalf("login %v %v", rep, err)
	}
	c.Close()

	h.stdin.Close()
	if err := h.wait(t); err != nil {
		t.Fatalf("run: %v", err)
	}
	select {
	case <-f.stopped:
	default:
		t.Fatal("engine still running after stdin closed")
	}
	if c, err := net.Dial("tcp", socksAddr); err == nil {
		c.Close()
		t.Fatal("SOCKS listener still open after stdin closed")
	}

	h.mu.Lock()
	out := h.all.String()
	h.mu.Unlock()
	for _, secret := range []string{testKey, testPass} {
		if strings.Contains(out, secret) {
			t.Fatalf("helper output contains a secret:\n%s", out)
		}
	}
}

func TestRunRefusesBadStart(t *testing.T) {
	for _, c := range []struct {
		name string
		m    proto.Msg
	}{
		{"no SOCKS login", start("fake", testUser, "")},
		{"unknown type", start("carrier-pigeon", testUser, testPass)},
	} {
		t.Run(c.name, func(t *testing.T) {
			useFake(t)
			h := startHelper(t)
			h.read(t) // hello
			h.send(t, c.m)
			if m := h.read(t); m.State != proto.Failed || m.Reason == "" {
				t.Fatalf("got %+v, want failed with a reason", m)
			}
			if err := h.wait(t); err == nil {
				t.Fatal("run succeeded")
			}
		})
	}
}

// asker is an engine that waits on a one-time code.
type asker struct {
	fake
	answers chan string
}

func (a *asker) Answer(code string) { a.answers <- code }

func TestRunForwardsAnswers(t *testing.T) {
	a := &asker{fake{stopped: make(chan struct{})}, make(chan string, 1)}
	engines["asker"] = func(string, map[string]string) (engine, error) { return a, nil }
	t.Cleanup(func() { delete(engines, "asker") })
	h := startHelper(t)
	h.read(t) // hello
	h.send(t, start("asker", testUser, testPass))
	h.read(t) // connecting
	h.read(t) // connected
	h.send(t, proto.Msg{T: proto.Answer, OTP: "123456"})
	select {
	case got := <-a.answers:
		if got != "123456" {
			t.Fatalf("engine got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer never reached the engine")
	}
	h.stdin.Close()
	h.wait(t)
}

// Every OpenConnect type runs, and none takes a secret in its settings.
func TestOpenConnectTypesRegistered(t *testing.T) {
	for _, typ := range []string{proto.TypeAnyConnect, proto.TypeSSLVPN, proto.TypeGlobalProtect, proto.TypePulse,
		proto.TypeNetworkConnect, proto.TypeF5, proto.TypeArray} {
		newEngine, ok := engines[typ]
		if !ok {
			t.Errorf("%s is not registered", typ)
			continue
		}
		if _, err := newEngine(`{"gateway":"vpn.example.test"}`, map[string]string{"password": "p"}); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
}
