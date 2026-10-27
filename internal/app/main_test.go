package app

import (
	"testing"

	"hostbay/internal/ai/claudecode/claudetest"
)

// TestMain lets this test binary stand in for `claude` when a Claude Code
// provider test starts the fake program (claudetest).
func TestMain(m *testing.M) { claudetest.Main(m) }
