//go:build manual

package secrets

import (
	"context"
	"testing"
)

// TestTouchIDPrompt shows the real macOS prompt; run it by hand with
// go test -tags manual -run TestTouchIDPrompt -v ./internal/secrets
// and report which Result each answer (pass, cancel, wrong password) gives.
func TestTouchIDPrompt(t *testing.T) {
	t.Logf("Result = %d (1 passed, 2 cancelled, 3 failed, 4 not configured)",
		NewVerifier(nil, 0, nil).Verify(context.Background(), "test Hostbay verification"))
}
