//go:build claudecode_live

package claudecode

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"hostbay/internal/ai"
)

// TestLiveRealClaudeCode runs the person's own Claude Code once, with their
// own sign-in. It is never run in CI: go test -tags claudecode_live -run Live
func TestLiveRealClaudeCode(t *testing.T) {
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not on PATH")
	}
	p := New(path)
	info, err := p.Test(t.Context())
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	t.Logf("version %s, signed in via %s", info.Version, info.Status.AuthMethod)

	var text strings.Builder
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	reply, err := p.Stream(ctx, ai.Request{
		Model:    "haiku",
		System:   "You are the assistant of a terminal tool.",
		Messages: []ai.Message{{Role: "user", Content: []ai.Block{{Type: "text", Text: "Check disk usage on the target terminal."}}}},
		Tools: []ai.Tool{{Name: "run_command", Description: "Run one shell command in the target terminal.",
			Schema: []byte(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)}},
	}, func(s string) { text.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("text %q, %d blocks, stop %s", text.String(), len(reply.Message.Content), reply.StopReason)
	if reply.StopReason != "tool_use" {
		t.Fatalf("the model did not call the tool: %+v", reply)
	}
}
