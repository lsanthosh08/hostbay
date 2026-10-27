// Package aitest is a fake AI provider for tests: an HTTP server that
// answers each request with the next scripted reply and records what it
// was sent. No test talks to a real provider.
package aitest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Reply is one scripted answer. Body is sent as it is, as server-sent
// events unless Status is not 200. Before is closed or nil; the reply
// waits on it after its first half, so a test can act mid-stream.
type Reply struct {
	Status int
	Body   string
	Before <-chan struct{}
}

// Request is what the server was sent.
type Request struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

// Server is the fake provider.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	replies  []Reply
	requests []Request
	// Models answers a model list request.
	Models []string
}

// New starts a fake provider that t closes.
func New(t testing.TB) *Server {
	s := &Server{Models: []string{"claude-opus-5-5", "claude-sonnet-5-5"}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Push queues replies, answered in order.
func (s *Server) Push(r ...Reply) {
	s.mu.Lock()
	s.replies = append(s.replies, r...)
	s.mu.Unlock()
}

// Requests is everything the server was sent so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	if strings.HasSuffix(r.URL.Path, "/models") {
		s.mu.Unlock()
		var l struct {
			Data []map[string]string `json:"data"`
		}
		for _, m := range s.Models {
			l.Data = append(l.Data, map[string]string{"id": m})
		}
		_ = json.NewEncoder(w).Encode(l)
		return
	}
	if len(s.replies) == 0 {
		s.mu.Unlock()
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"no reply scripted"}}`, http.StatusBadRequest)
		return
	}
	rep := s.replies[0]
	s.replies = s.replies[1:]
	s.mu.Unlock()
	if rep.Status != 0 && rep.Status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.Status)
		io.WriteString(w, rep.Body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	half := len(rep.Body) / 2
	if rep.Before != nil {
		half = strings.LastIndex(rep.Body[:half], "\n\n") + 2
	}
	io.WriteString(w, rep.Body[:half])
	w.(http.Flusher).Flush()
	if rep.Before != nil {
		select {
		case <-rep.Before:
		case <-r.Context().Done():
			return
		}
	}
	io.WriteString(w, rep.Body[half:])
}

func event(typ string, v any) string {
	b, _ := json.Marshal(v)
	return "event: " + typ + "\ndata: " + string(b) + "\n\n"
}

// AnthropicText is a Messages stream that says text and ends the turn.
func AnthropicText(text string) string {
	return anthropic("end_turn",
		event("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}),
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}),
		event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
	)
}

// AnthropicTool is a Messages stream that calls tool name with input
// (JSON), sent in three pieces as input_json_delta events.
func AnthropicTool(id, name, input string) string {
	return anthropic("tool_use", AnthropicToolBlock(0, id, name, input)...)
}

// AnthropicToolBlock is the events of one tool_use block at index.
func AnthropicToolBlock(index int, id, name, input string) []string {
	out := []string{event("content_block_start", map[string]any{"type": "content_block_start", "index": index,
		"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}})}
	third := len(input)/3 + 1
	for i := 0; i < len(input); i += third {
		out = append(out, event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": input[i:min(len(input), i+third)]}}))
	}
	return append(out, event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}))
}

// Command is the JSON input of a run_command call.
func Command(cmd string) string {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return string(b)
}

func anthropic(stop string, blocks ...string) string {
	return event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_test", "type": "message", "role": "assistant", "content": []any{}}}) +
		strings.Join(blocks, "") +
		event("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}}) +
		event("message_stop", map[string]any{"type": "message_stop"})
}

// AnthropicError is a Messages stream that stops with an error event after
// half of a tool call.
func AnthropicError(typ, message string) string {
	half := AnthropicToolBlock(0, "toolu_half", "run_command", Command("rm -rf /tmp/x"))
	return event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_test"}}) +
		strings.Join(half[:2], "") +
		event("error", map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": message}})
}

// Overloaded is the body of an HTTP 529.
var Overloaded = fmt.Sprintf(`{"type":"error","error":{"type":"overloaded_error","message":%q}}`, "Overloaded")
