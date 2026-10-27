package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"hostbay/internal/ai/aitest"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/ai/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func client(t *testing.T, protocol, base, key string) Provider {
	t.Helper()
	p, err := New(protocol, base, key, "Hostbay/test", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var testTools = []Tool{{Name: "run_command", Description: "run", Schema: json.RawMessage(`{"type":"object"}`)}}

// The recorded stream's tool input arrives in input_json_delta pieces and is
// read whole at content_block_stop; text streams as it comes; ping and
// unknown events are ignored, and the thinking block is kept to send back.
func TestAnthropicStreamsTextAndJoinsToolInput(t *testing.T) {
	srv := aitest.New(t)
	srv.Push(aitest.Reply{Body: fixture(t, "anthropic.sse")})
	var streamed strings.Builder
	r, err := client(t, Anthropic, srv.URL, "sk-ant-sample").Stream(context.Background(),
		Request{Model: "claude-opus-5-5", System: "sys", Messages: []Message{{Role: "user", Content: []Block{{Type: "text", Text: "load?"}}}}, Tools: testTools},
		func(s string) { streamed.WriteString(s) })
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "Checking the load." || r.StopReason != "tool_use" || len(r.Message.Content) != 3 {
		t.Fatalf("reply = %+v, streamed %q", r, streamed.String())
	}
	think, text, call := r.Message.Content[0], r.Message.Content[1], r.Message.Content[2]
	if think.Type != "opaque" || !strings.Contains(string(think.Opaque), `"signature":"EqQBCkgIAR"`) {
		t.Fatalf("thinking block = %s", think.Opaque)
	}
	var in struct{ Command string }
	if text.Text != "Checking the load." || call.Type != "tool_use" || call.ToolID != "toolu_01" || call.Name != "run_command" ||
		json.Unmarshal(call.Input, &in) != nil || in.Command != "uptime" {
		t.Fatalf("blocks = %+v %+v", text, call)
	}

	req := srv.Requests()[0]
	if req.Path != "/v1/messages" || req.Header.Get("x-api-key") != "sk-ant-sample" || req.Header.Get("anthropic-version") != anthropicVersion ||
		req.Header.Get("User-Agent") != "Hostbay/test" || req.Header.Get("Authorization") != "" {
		t.Fatalf("request %s %v", req.Path, req.Header)
	}

	// The thinking block goes back unchanged with the tool result.
	srv.Push(aitest.Reply{Body: aitest.AnthropicText("done")})
	next := []Message{{Role: "user", Content: []Block{{Type: "text", Text: "load?"}}}, r.Message,
		{Role: "user", Content: []Block{{Type: "tool_result", ToolID: "toolu_01", Text: "up 3 days"}}}}
	if _, err := client(t, Anthropic, srv.URL, "k").Stream(context.Background(), Request{Model: "m", Messages: next, Tools: testTools}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(srv.Requests()[1].Body, &sent); err != nil {
		t.Fatal(err)
	}
	if b := sent.Messages[1].Content[0]; b["type"] != "thinking" || b["signature"] != "EqQBCkgIAR" {
		t.Fatalf("sent back %v", b)
	}
	if b := sent.Messages[2].Content[0]; b["type"] != "tool_result" || b["tool_use_id"] != "toolu_01" || b["content"] != "up 3 days" {
		t.Fatalf("tool result sent as %v", b)
	}
}

// A 529, an error event after half a tool call, or a stream that just
// stops: each is an error and no reply, so nothing from it can run.
func TestAnthropicFailedStreamsGiveNoReply(t *testing.T) {
	srv := aitest.New(t)
	cut := aitest.AnthropicTool("toolu_1", "run_command", aitest.Command("ls"))
	cut = cut[:strings.Index(cut, "event: content_block_stop")]
	srv.Push(
		aitest.Reply{Status: 529, Body: aitest.Overloaded},
		aitest.Reply{Body: aitest.AnthropicError("overloaded_error", "Overloaded")},
		aitest.Reply{Body: cut},
	)
	p := client(t, Anthropic, srv.URL, "sk-ant-secret")
	for i, want := range []string{"529", "overloaded_error", "ended before"} {
		r, err := p.Stream(context.Background(), Request{Model: "m", Tools: testTools}, func(string) {})
		if err == nil || !strings.Contains(err.Error(), want) || len(r.Message.Content) != 0 {
			t.Fatalf("stream %d = %+v, %v", i, r, err)
		}
		if strings.Contains(err.Error(), "sk-ant-secret") {
			t.Fatal("the key is in the error")
		}
		var se *StatusError
		if i < 2 && !errors.As(err, &se) {
			t.Fatalf("stream %d error %T", i, err)
		}
	}
}

func TestAnthropicListsModels(t *testing.T) {
	srv := aitest.New(t)
	ids, err := client(t, Anthropic, srv.URL, "k").Models(context.Background())
	if err != nil || strings.Join(ids, ",") != "claude-opus-5-5,claude-sonnet-5-5" || srv.Requests()[0].Path != "/v1/models" {
		t.Fatalf("models = %v, %v", ids, err)
	}
}
