package app

import (
	"testing"

	"hostbay/internal/ai/claudecode/claudetest"
	"hostbay/internal/vpn/vpntest"
)

// TestMain lets this test binary stand in for `claude` when a Claude Code
// provider test starts the fake program (claudetest), and for hostbay-vpn
// when a VPN test starts its fake helper (vpntest).
func TestMain(m *testing.M) {
	vpntest.MaybeRun()
	claudetest.Main(m)
}
