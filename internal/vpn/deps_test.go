package vpn_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The GUI binary must not link the userspace VPN engines; they run in the
// helper processes only.
func TestMainBinaryHasNoVPNEngineDeps(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	out, err := exec.Command(goBin, "list", "-deps", "hostbay").Output()
	if err != nil {
		t.Fatalf("go list -deps hostbay: %v", err)
	}
	for dep := range strings.Lines(string(out)) {
		for _, banned := range []string{"wireguard", "gvisor", "tailscale.com"} {
			if strings.Contains(dep, banned) {
				t.Errorf("hostbay depends on %s", strings.TrimSpace(dep))
			}
		}
	}
}
