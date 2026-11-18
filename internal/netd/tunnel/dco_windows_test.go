package tunnel

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"hostbay/internal/netd"
)

// The helper reuses its ovpn-dco adapter and creates it only when missing.
func TestDCOAdapterCreatedOnlyWhenMissing(t *testing.T) {
	e := newEnv(t)
	os.WriteFile(netd.ClientPath(e.dir, "tapctl"), []byte("x"), 0o755)
	var calls [][]string
	have := ""
	orig := runTapctl
	t.Cleanup(func() { runTapctl = orig })
	runTapctl = func(_ string, _ []string, args ...string) (string, error) {
		calls = append(calls, args)
		if args[0] == "create" {
			have = "{0A1B}\t" + dcoName + "\r\n"
		}
		return "{9F9F}\tOpenVPN Data Channel Offload\r\n" + have, nil
	}
	for _, h := range []string{"t1", "t2"} {
		if name, err := e.m.dcoAdapter(h); err != nil || name != dcoName {
			t.Fatalf("adapter %q, %v", name, err)
		}
		e.m.cleanup(h)
	}
	creates := slices.IndexFunc(calls, func(c []string) bool { return c[0] == "create" })
	if creates < 0 || strings.Join(calls[creates], " ") != "create --hwid ovpn-dco --name "+dcoName || len(calls) != 3 {
		t.Fatalf("tapctl calls %q", calls)
	}
}

// A second AWS SSO tunnel is refused while the first holds the adapter, and
// may start once it has ended.
func TestDCOAdapterHeldByOneTunnel(t *testing.T) {
	e := newEnv(t)
	os.WriteFile(netd.ClientPath(e.dir, "tapctl"), []byte("x"), 0o755)
	orig := runTapctl
	t.Cleanup(func() { runTapctl = orig })
	runTapctl = func(string, []string, ...string) (string, error) { return "{0A1B}\t" + dcoName + "\r\n", nil }
	if _, err := e.m.dcoAdapter("t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.dcoAdapter("t2"); !errors.Is(err, errDCOBusy) {
		t.Fatalf("second tunnel: %v", err)
	}
	e.m.cleanup("t1")
	if _, err := e.m.dcoAdapter("t2"); err != nil {
		t.Fatalf("after the first ended: %v", err)
	}
}
