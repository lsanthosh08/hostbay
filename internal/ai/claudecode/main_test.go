package claudecode

import (
	"encoding/json"
	"strings"
	"testing"

	"hostbay/internal/ai"
	"hostbay/internal/ai/claudecode/claudetest"
)

func TestMain(m *testing.M) { claudetest.Main(m) }

const testTag = "hb-7f3a9c"

// stream builds the stream-json a program would print for a reply made of
// these text deltas, then finishing normally.
func stream(deltas ...string) string {
	var b strings.Builder
	line := func(v any) {
		j, _ := json.Marshal(v)
		b.Write(j)
		b.WriteByte('\n')
	}
	line(map[string]any{"type": "system", "subtype": "init", "claude_code_version": "2.1.310",
		"tools": []any{}, "mcp_servers": []any{}, "skills": []any{}, "slash_commands": []any{},
		"plugins": []any{map[string]any{"name": "example-plugin"}}})
	var full strings.Builder
	for _, d := range deltas {
		full.WriteString(d)
		line(map[string]any{"type": "stream_event", "event": map[string]any{
			"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": d}}})
	}
	line(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": full.String()}}}})
	line(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": full.String()})
	return b.String()
}

func block(call string) string { return "<" + testTag + ">" + call + "</" + testTag + ">" }

const runDF = `{"name":"run_command","input":{"command":"df -h"}}`

var runTool = ai.Tool{Name: "run_command", Description: "Run one command.", Schema: json.RawMessage(`{"type":"object"}`)}

func userText(s string) ai.Message {
	return ai.Message{Role: "user", Content: []ai.Block{{Type: "text", Text: s}}}
}

// newProvider returns a provider for a fake program scripted by s, with the
// tag fixed and the environment the test gives.
func newProvider(t *testing.T, s claudetest.Script, environ ...string) (*Provider, *claudetest.Fake) {
	t.Helper()
	f := claudetest.Install(t, s)
	p := New(f.Path)
	p.tagFn = func() string { return testTag }
	if environ != nil {
		p.environ = func() []string { return environ }
	}
	return p, f
}

// collect streams one request and returns the reply, the text the page got
// and the error.
func collect(t *testing.T, p *Provider, r ai.Request) (ai.Reply, []string, error) {
	t.Helper()
	var pieces []string
	reply, err := p.Stream(t.Context(), r, func(s string) { pieces = append(pieces, s) })
	return reply, pieces, err
}

func req(msgs ...ai.Message) ai.Request {
	return ai.Request{Model: "haiku", System: "You are the assistant.", Messages: msgs, Tools: []ai.Tool{runTool}}
}
