package tailnet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/logtail"
	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"

	"hostbay/internal/vpn/proto"
)

// startControl runs an in-process control server with its own DERP and STUN
// on loopback, as tsnet's own tests do.
func startControl(t *testing.T, requireAuth bool) *testcontrol.Server {
	t.Helper()
	netns.SetEnabled(false) // tests bind no sockets to interfaces
	t.Cleanup(func() { netns.SetEnabled(true) })
	c := &testcontrol.Server{
		DERPMap:     integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1"),
		DNSConfig:   &tailcfg.DNSConfig{Proxied: true},
		RequireAuth: requireAuth,
		Logf:        logger.Discard,
	}
	c.HTTPTestServer = httptest.NewServer(c)
	t.Cleanup(c.HTTPTestServer.Close)
	return c
}

func conf(t *testing.T, dir, controlURL string) string {
	b, err := json.Marshal(map[string]string{"dir": dir, "controlURL": controlURL, "hostname": filepath.Base(dir)})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// running is an engine's Run in the background with every message it
// reports.
type running struct {
	msgs chan proto.Msg
	stop func()
}

func start(t *testing.T, config string, secrets map[string]string) (*Engine, *running) {
	t.Helper()
	e, err := New(config, secrets)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{msgs: make(chan proto.Msg, 1000)}
	done := make(chan struct{})
	go func() {
		e.Run(ctx, func(m proto.Msg) { r.msgs <- m })
		close(done)
	}()
	r.stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("Run did not return after its context ended")
		}
	}
	t.Cleanup(r.stop)
	return e, r
}

// await returns the first message matching ok, failing on a failed or
// dropped state. seen gets every message read on the way, including the
// match.
func (r *running) await(t *testing.T, what string, ok func(proto.Msg) bool, seen func(proto.Msg)) proto.Msg {
	t.Helper()
	timeout := time.After(60 * time.Second)
	for {
		select {
		case m := <-r.msgs:
			if seen != nil {
				seen(m)
			}
			if m.State == proto.Failed || m.State == proto.Dropped {
				t.Fatalf("waiting for %s: %s: %s", what, m.State, m.Reason)
			}
			if ok(m) {
				return m
			}
		case <-timeout:
			t.Fatalf("no %s within 60s", what)
		}
	}
}

func connected(m proto.Msg) bool { return m.State == proto.Connected }

// lastNodeState keeps the newest nodeState message's store.
func lastNodeState(t *testing.T, into *string) func(proto.Msg) {
	return func(m proto.Msg) {
		if m.T != proto.NodeState {
			return
		}
		if _, err := base64.StdEncoding.DecodeString(m.NodeState); err != nil {
			t.Errorf("nodeState is not base64: %v", err)
		}
		*into = m.NodeState
	}
}

// privateKeyPattern matches a private key as the store writes it, inside
// whichever entry holds it: the machine key, the node's current and old keys
// in its profile, and its tailnet lock key.
var privateKeyPattern = regexp.MustCompile(`(?:privkey|nlpriv):([0-9a-f]{64})`)

// privateKeys is every non-zero private key in a nodeState, in hex. A joined
// node has at least its machine key and its node key.
func privateKeys(t *testing.T, nodeState string) map[string]bool {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(nodeState)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string][]byte
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	ks := map[string]bool{}
	for _, v := range m {
		for _, k := range privateKeyPattern.FindAllSubmatch(v, -1) {
			if string(k[1]) != strings.Repeat("0", 64) {
				ks[string(k[1])] = true
			}
		}
	}
	if len(ks) < 2 {
		t.Fatalf("nodeState holds %d private keys, want the machine key and the node key", len(ks))
	}
	return ks
}

// noKeysOnDisk fails when any file under root holds one of the keys, in hex
// or raw.
func noKeysOnDisk(t *testing.T, root string, keys map[string]bool) {
	t.Helper()
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for k := range keys {
			raw, _ := hex.DecodeString(k)
			if bytes.Contains(b, []byte(k)) || bytes.Contains(b, raw) {
				t.Errorf("%s holds a private key of the node", path)
			}
		}
		return nil
	})
}

// Nothing the helper logs is uploaded to Tailscale's log service once an
// engine exists, which is before any node starts.
func TestNewDisablesLogUpload(t *testing.T) {
	var uploads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { uploads.Add(1) }))
	defer srv.Close()

	if _, err := New(conf(t, t.TempDir(), ""), nil); err != nil {
		t.Fatal(err)
	}
	if !envknob.NoLogsNoSupport() {
		t.Error("TS_NO_LOGS_NO_SUPPORT is not set")
	}
	lg := logtail.NewLogger(logtail.Config{Collection: "test.hostbay", BaseURL: srv.URL, HTTPC: srv.Client(), Stderr: io.Discard}, t.Logf)
	lg.Logf("a log line")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lg.Shutdown(ctx)
	if n := uploads.Load(); n != 0 {
		t.Errorf("logtail uploaded %d times", n)
	}
}

// Without an auth key or saved state the engine asks for a browser sign-in,
// connects once it is done, and hands its node state to Hostbay; started
// again with that state it connects with no sign-in. Nothing it writes, in
// its folder or anywhere else, holds the node's keys.
func TestSignInThenReconnectFromNodeState(t *testing.T) {
	home := t.TempDir() // where tsnet would put a default state folder
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("AppData", home)
	control := startControl(t, true)
	dir := filepath.Join(t.TempDir(), "profile")

	var state string
	_, r := start(t, conf(t, dir, control.HTTPTestServer.URL), nil)
	need := r.await(t, "a browser sign-in", func(m proto.Msg) bool { return m.T == proto.Need }, lastNodeState(t, &state))
	if need.Need != proto.NeedBrowser || need.URL == "" {
		t.Fatalf("need = %+v, want a browser URL", need)
	}
	if !control.CompleteAuth(need.URL) {
		t.Fatalf("control does not know the sign-in URL %s", need.URL)
	}
	up := r.await(t, "connected", connected, lastNodeState(t, &state))
	if len(up.Address) == 0 || len(up.Routes) == 0 {
		t.Errorf("connected without addresses or routes: %+v", up)
	}
	r.stop()
	for len(r.msgs) > 0 { // what the node saved while shutting down
		lastNodeState(t, &state)(<-r.msgs)
	}
	keys := privateKeys(t, state)

	_, r = start(t, conf(t, dir, control.HTTPTestServer.URL), map[string]string{"nodeState": state})
	again := r.await(t, "connected from saved state", func(m proto.Msg) bool {
		if m.T == proto.Need {
			t.Fatalf("asked to sign in again: %+v", m)
		}
		return connected(m)
	}, nil)
	if again.Address[0] != up.Address[0] {
		t.Errorf("reconnected as %v, was %v", again.Address, up.Address)
	}
	r.stop()

	noKeysOnDisk(t, dir, keys)
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Errorf("the engine wrote outside its folder: %v", entries)
	}
}

// Two profiles are two nodes: separate folders, separate keys, and each
// reaches the other through the tailnet.
func TestTwoProfilesAreSeparateNodes(t *testing.T) {
	control := startControl(t, false)
	dirA, dirB := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	var stateA, stateB string
	a, ra := start(t, conf(t, dirA, control.HTTPTestServer.URL), map[string]string{"authKey": "unused-by-testcontrol"})
	b, rb := start(t, conf(t, dirB, control.HTTPTestServer.URL), nil)
	upA := ra.await(t, "a connected", connected, lastNodeState(t, &stateA))
	upB := rb.await(t, "b connected", connected, lastNodeState(t, &stateB))
	if upA.Address[0] == upB.Address[0] {
		t.Errorf("both profiles have address %s", upA.Address[0])
	}
	keysA, keysB := privateKeys(t, stateA), privateKeys(t, stateB)
	for k := range keysA {
		if keysB[k] {
			t.Error("both profiles share a private key")
		}
	}

	ln, err := b.srv.Listen("tcp", ":7")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	ip, _, _ := net.ParseCIDR(upB.Address[0])
	pc, err := b.srv.ListenPacket("udp", net.JoinHostPort(ip.String(), "7"))
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], from)
		}
	}()

	c := dialRetry(t, a, net.JoinHostPort(ip.String(), "7"))
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "ping" {
		t.Fatalf("echo = %q, %v", got, err)
	}

	u, err := a.Dial(context.Background(), "udp", net.JoinHostPort(ip.String(), "7"))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	u.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := u.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	if n, err := u.Read(got); err != nil || string(got[:n]) != "pong" {
		t.Fatalf("UDP echo = %q, %v", got[:n], err)
	}

	noKeysOnDisk(t, dirA, keysA)
	noKeysOnDisk(t, dirB, keysB)
}

// dialRetry dials until the peers have found each other through DERP.
func dialRetry(t *testing.T, e *Engine, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := e.Dial(ctx, "tcp", addr)
		cancel()
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("dialing %s: %v", addr, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// A Tailscale profile joins only the tailnet its own settings name, whatever
// the environment Hostbay passes on says.
func TestNewIgnoresTailscaleEnvironment(t *testing.T) {
	t.Setenv("TS_CONTROL_URL", "https://other.example.test")
	t.Setenv("TS_AUTHKEY", "tskey-someone-else")
	if _, err := New(conf(t, t.TempDir(), ""), nil); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"TS_CONTROL_URL", "TS_AUTHKEY"} {
		if v := os.Getenv(k); v != "" {
			t.Errorf("%s is still %q", k, v)
		}
	}
}

func TestNewRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		config  string
		secrets map[string]string
	}{
		"no folder":       {`{"controlURL":"https://hs.example.test"}`, nil},
		"not JSON":        {`dir = /tmp`, nil},
		"bad saved state": {conf(t, t.TempDir(), ""), map[string]string{"nodeState": "not base64!"}},
	} {
		if _, err := New(c.config, c.secrets); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}
