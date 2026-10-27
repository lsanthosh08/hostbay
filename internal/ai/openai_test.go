package ai

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"hostbay/internal/ai/aitest"
)

// One stream per OpenAI-compatible family, authored from each one's
// documented chunk shape: tool call fragments are joined by index, and
// OpenRouter's comment lines and vLLM's usage chunk are skipped.
func TestOpenAICompatibleStreamsJoinToolCalls(t *testing.T) {
	for _, name := range []string{"openai.sse", "ollama.sse", "lmstudio.sse", "vllm.sse", "openrouter.sse"} {
		t.Run(name, func(t *testing.T) {
			srv := aitest.New(t)
			srv.Push(aitest.Reply{Body: fixture(t, name)})
			var streamed strings.Builder
			r, err := client(t, OpenAIChat, srv.URL+"/v1", "sk-sample").Stream(context.Background(),
				Request{Model: "m", System: "sys", Messages: []Message{{Role: "user", Content: []Block{{Type: "text", Text: "load?"}}}}, Tools: testTools},
				func(s string) { streamed.WriteString(s) })
			if err != nil {
				t.Fatal(err)
			}
			if streamed.String() != "Checking the load." || r.StopReason != "tool_calls" || len(r.Message.Content) != 2 {
				t.Fatalf("reply = %+v, streamed %q", r, streamed.String())
			}
			call := r.Message.Content[1]
			var in struct{ Command string }
			if call.Type != "tool_use" || call.Name != "run_command" || call.ToolID == "" || json.Unmarshal(call.Input, &in) != nil || in.Command != "uptime" {
				t.Fatalf("call = %+v", call)
			}
			req := srv.Requests()[0]
			if req.Path != "/v1/chat/completions" || req.Header.Get("Authorization") != "Bearer sk-sample" || req.Header.Get("x-api-key") != "" {
				t.Fatalf("request %s %v", req.Path, req.Header)
			}
		})
	}
}

// A server that sends whole calls without an index still gives each its
// own call; one that never says how it finished gave no reply.
func TestOpenAIWithoutIndexAndCutShort(t *testing.T) {
	srv := aitest.New(t)
	chunk := func(s string) string { return "data: " + s + "\n\n" }
	srv.Push(
		aitest.Reply{Body: chunk(`{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"run_command","arguments":"{\"command\":\"uptime\"}"}}]}}]}`) +
			chunk(`{"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"read_output","arguments":"{}"}}]}}]}`) +
			chunk(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`) + chunk("[DONE]")},
		aitest.Reply{Body: chunk(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"run_command","arguments":"{\"comm"}}]}}]}`)},
	)
	p := client(t, OpenAIChat, srv.URL, "")
	r, err := p.Stream(context.Background(), Request{Model: "m"}, func(string) {})
	if err != nil || len(r.Message.Content) != 2 || r.Message.Content[0].ToolID != "a" || r.Message.Content[1].Name != "read_output" {
		t.Fatalf("reply = %+v, %v", r, err)
	}
	if srv.Requests()[0].Header.Get("Authorization") != "" {
		t.Fatal("an empty key was sent")
	}
	if r, err := p.Stream(context.Background(), Request{Model: "m"}, func(string) {}); err == nil || len(r.Message.Content) != 0 {
		t.Fatalf("cut stream = %+v, %v", r, err)
	}
}

// Tool results go back as tool messages, and calls as tool_calls.
func TestOpenAISendsToolTurns(t *testing.T) {
	srv := aitest.New(t)
	srv.Push(aitest.Reply{Body: "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"})
	msgs := []Message{
		{Role: "user", Content: []Block{{Type: "text", Text: "load?"}}},
		{Role: "assistant", Content: []Block{{Type: "tool_use", ToolID: "c1", Name: "run_command", Input: json.RawMessage(`{"command":"uptime"}`)}}},
		{Role: "user", Content: []Block{{Type: "tool_result", ToolID: "c1", Text: "up"}}},
	}
	if _, err := client(t, OpenAIChat, srv.URL, "k").Stream(context.Background(), Request{Model: "m", System: "sys", Messages: msgs}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	var sent struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(srv.Requests()[0].Body, &sent)
	if len(sent.Messages) != 4 || sent.Messages[0]["role"] != "system" || sent.Messages[2]["tool_calls"] == nil ||
		sent.Messages[3]["role"] != "tool" || sent.Messages[3]["tool_call_id"] != "c1" {
		t.Fatalf("sent %v", sent.Messages)
	}
}
