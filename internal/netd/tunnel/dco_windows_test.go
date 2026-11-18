package tunnel

import (
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
	for range 2 {
		if name, err := e.m.dcoAdapter(); err != nil || name != dcoName {
			t.Fatalf("adapter %q, %v", name, err)
		}
	}
	creates := slices.IndexFunc(calls, func(c []string) bool { return c[0] == "create" })
	if creates < 0 || strings.Join(calls[creates], " ") != "create --hwid ovpn-dco --name "+dcoName || len(calls) != 3 {
		t.Fatalf("tapctl calls %q", calls)
	}
}
