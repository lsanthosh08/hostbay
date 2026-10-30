package vpn

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/vpn/proto"
	"hostbay/internal/vpn/vpntest"
)

func TestMain(m *testing.M) {
	vpntest.MaybeRun()
	os.Exit(m.Run())
}

var testStart = proto.Msg{
	T: proto.Start, Type: proto.TypeWireGuard, Config: "[Interface]\n",
	Secrets: map[string]string{"privateKey": "fake-key"}, SocksUser: "u1", SocksPass: "p1",
}

// start runs a helper in mode and returns it with the channel its reports go to.
func start(t *testing.T, mode string) (*vpntest.Fake, *Helper, chan proto.Msg, error) {
	t.Helper()
	f := vpntest.Install(t, mode)
	msgs := make(chan proto.Msg, 16)
	h, err := Start(context.Background(), f.Path, testStart, func(m proto.Msg) { msgs <- m })
	if h != nil {
		t.Cleanup(func() { h.Close(); <-h.Done() })
	}
	return f, h, msgs, err
}

func recv(t *testing.T, c chan proto.Msg) proto.Msg {
	t.Helper()
	select {
	case m := <-c:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		panic("unreachable")
	}
}

func TestStartSendsStartAndReportsStatesInOrder(t *testing.T) {
	f, _, msgs, err := start(t, vpntest.OK)
	if err != nil {
		t.Fatal(err)
	}
	if m := recv(t, msgs); m.State != proto.Connecting {
		t.Fatalf("first report %+v, want connecting", m)
	}
	m := recv(t, msgs)
	if m.State != proto.Connected || m.SocksPort != vpntest.Port || m.Address[0] != vpntest.Address {
		t.Fatalf("second report %+v, want connected", m)
	}
	got := f.Starts(t)
	if len(got) != 1 || got[0].SocksUser != "u1" || got[0].SocksPass != "p1" || got[0].Secrets["privateKey"] != "fake-key" {
		t.Fatalf("helper was sent %+v", got)
	}
}

func TestAnswerSendsTheOneTimeCode(t *testing.T) {
	_, h, msgs, err := start(t, vpntest.OTP)
	if err != nil {
		t.Fatal(err)
	}
	recv(t, msgs) // connecting
	if m := recv(t, msgs); m.T != proto.Need || m.Need != proto.NeedOTP {
		t.Fatalf("report %+v, want need otp", m)
	}
	if err := h.Answer(vpntest.Code); err != nil {
		t.Fatal(err)
	}
	recv(t, msgs) // connecting again
	if m := recv(t, msgs); m.State != proto.Connected {
		t.Fatalf("report %+v, want connected", m)
	}
}

func TestHelperExitingUnexpectedlyReportsDropped(t *testing.T) {
	_, h, msgs, err := start(t, vpntest.Crash)
	if err != nil {
		t.Fatal(err)
	}
	recv(t, msgs) // connecting
	recv(t, msgs) // connected
	m := recv(t, msgs)
	if m.State != proto.Dropped || !strings.Contains(m.Reason, "stopped unexpectedly") {
		t.Fatalf("report %+v, want dropped saying it stopped unexpectedly", m)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed")
	}
}

func TestCloseEndsTheHelperWithoutAReport(t *testing.T) {
	_, h, msgs, err := start(t, vpntest.OK)
	if err != nil {
		t.Fatal(err)
	}
	recv(t, msgs)
	recv(t, msgs)
	h.Close()
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit after Close")
	}
	select {
	case m := <-msgs:
		t.Fatalf("closing reported %+v", m)
	default:
	}
}

func TestMissingOrMismatchedHelperIsHelperMissing(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })

	_, err := Start(context.Background(), filepath.Join(t.TempDir(), HelperName()), testStart, func(proto.Msg) {})
	if apperr.CodeOf(err) != apperr.HelperMissing || !strings.Contains(err.Error(), "Reinstall") {
		t.Fatalf("missing helper: %v", err)
	}
	for _, mode := range []string{vpntest.BadVersion, vpntest.Silent} {
		_, h, _, err := start(t, mode)
		if h != nil || apperr.CodeOf(err) != apperr.HelperMissing || !strings.Contains(err.Error(), "Reinstall") {
			t.Fatalf("%s: helper %v, err %v", mode, h, err)
		}
	}
}

func TestHelperPathIsBesideTheExecutable(t *testing.T) {
	p, err := HelperPath()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	if p != filepath.Join(filepath.Dir(exe), HelperName()) {
		t.Fatalf("HelperPath %q", p)
	}
}

// A Tailscale node's state reaches Hostbay, including what it writes while
// closing, and is not taken for a tunnel state.
func TestNodeStatePassesThroughEvenWhileClosing(t *testing.T) {
	_, h, msgs, err := start(t, vpntest.Node)
	if err != nil {
		t.Fatal(err)
	}
	recv(t, msgs) // connecting
	recv(t, msgs) // connected
	if m := recv(t, msgs); m.T != proto.NodeState || m.NodeState != vpntest.NodeUp {
		t.Fatalf("report %+v, want the node state", m)
	}
	h.Close()
	if m := recv(t, msgs); m.T != proto.NodeState || m.NodeState != vpntest.NodeClosing {
		t.Fatalf("report %+v, want the closing node state", m)
	}
	<-h.Done()
	select {
	case m := <-msgs:
		t.Fatalf("report after close %+v", m)
	default:
	}
}
