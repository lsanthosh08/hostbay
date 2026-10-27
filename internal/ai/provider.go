// Package ai runs the AI agent in Go (KTD31): two small provider clients,
// Anthropic Messages and OpenAI-compatible Chat Completions, the
// conversation loop and what it sends. The provider key never leaves Go,
// and the agent reaches a terminal only through internal/agentexec's gate
// (KTD32).
package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"hostbay/internal/store"
)

// Message is one turn in Hostbay's own form; each client turns it into its
// protocol's.
type Message struct {
	Role    string  `json:"role"` // "user" or "assistant"
	Content []Block `json:"content"`
}

// Block is part of a message.
type Block struct {
	Type    string          `json:"type"` // "text", "tool_use", "tool_result" or "opaque"
	Text    string          `json:"text,omitempty"`
	ToolID  string          `json:"toolId,omitempty"` // tool_use: its ID; tool_result: the call it answers
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	IsError bool            `json:"isError,omitempty"`
	// Opaque is a block of the provider's own kind, such as an Anthropic
	// thinking block, sent back byte for byte.
	Opaque json.RawMessage `json:"opaque,omitempty"`
}

// Tool is one tool the model may call.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Request is one model call.
type Request struct {
	Model    string
	System   string
	Messages []Message
	Tools    []Tool
}

// Reply is a complete assistant turn. A stream that ends early is an
// error, never a Reply, so no tool call from a partial reply runs.
type Reply struct {
	Message    Message
	StopReason string
}

// Provider is one protocol's client.
type Provider interface {
	// Stream sends r and calls text with each piece of reply text as it
	// arrives.
	Stream(ctx context.Context, r Request, text func(string)) (Reply, error)
	Models(ctx context.Context) ([]string, error)
}

// Protocols.
const (
	Anthropic  = "anthropic"
	OpenAIChat = "openai-chat"
	// ClaudeCode is the person's own signed-in Claude Code program, run as
	// an inference-only subprocess. internal/ai/claudecode builds it; New
	// does not, because it takes a program path, not a URL and key.
	ClaudeCode = store.AIClaudeCode
)

// DefaultModels are offered before a provider lists its own. The first is
// the default.
var DefaultModels = map[string][]string{
	Anthropic:  {"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"},
	OpenAIChat: {},
	ClaudeCode: {"sonnet", "opus", "haiku", "fable"},
}

// New returns the client for protocol at baseURL. key may be empty for a
// local server that takes none.
func New(protocol, baseURL, key, userAgent string, c *http.Client) (Provider, error) {
	base := strings.TrimRight(baseURL, "/")
	switch protocol {
	case Anthropic:
		return &anthropic{base: base, key: key, ua: userAgent, c: c}, nil
	case OpenAIChat:
		return &openAI{base: base, key: key, ua: userAgent, c: c}, nil
	case ClaudeCode:
		return nil, errors.New("a Claude Code provider is set up from its program, not a URL and key")
	}
	return nil, fmt.Errorf("unknown provider protocol %q", protocol)
}

// do sends a request and returns its body, or the provider's refusal.
// The body is watched: idle too long or too large, the request ends.
func do(ctx context.Context, c *http.Client, method, url string, header http.Header, body []byte) (io.Reader, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	req.Header = header
	resp, err := c.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer cancel()
		defer resp.Body.Close()
		return nil, nil, statusError(resp)
	}
	w := newWatchdog(resp.Body, cancel)
	return w, func() { w.stop(); resp.Body.Close(); cancel() }, nil
}

// errorFields reads {"error":{"type","message"}}, {"error":"..."} or
// {"message":"..."} from an error body or stream event.
func errorFields(body []byte) (typ, msg string) {
	var e struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(body, &e) != nil {
		return "", ""
	}
	var inner struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(e.Error, &inner) == nil && (inner.Type != "" || inner.Message != "") {
		return inner.Type, clip(inner.Message, 500)
	}
	var s string
	if json.Unmarshal(e.Error, &s) == nil {
		return "", clip(s, 500)
	}
	return "", clip(e.Message, 500)
}

// readSSE calls fn with each server-sent event's name and data. Comment
// lines are skipped and no line may pass maxLine.
func readSSE(r io.Reader, fn func(event, data string) (bool, error)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var event string
	var data []string
	for {
		line, err := readLine(br)
		if err != nil {
			return err
		}
		switch {
		case line == "":
			if len(data) > 0 {
				done, err := fn(event, strings.Join(data, "\n"))
				if err != nil || done {
					return err
				}
			}
			event, data = "", nil
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
}

var errLineTooLong = errors.New("the provider sent a line longer than 1 MiB")

func readLine(br *bufio.Reader) (string, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				return string(buf), nil
			}
			return "", err
		}
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return "", errLineTooLong
		}
		if !isPrefix {
			return string(buf), nil
		}
	}
}

// errEarlyEnd is a stream that stopped before its last event.
var errEarlyEnd = errors.New("the provider's reply ended before it was complete")

// streamErr is why an unfinished stream stopped.
func streamErr(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errEarlyEnd
	}
	return err
}

// modelIDs reads the {"data":[{"id":...}]} list both protocols return.
func modelIDs(r io.Reader) ([]string, error) {
	var l struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(r, maxStream)).Decode(&l); err != nil {
		return nil, fmt.Errorf("the provider's model list cannot be read: %w", err)
	}
	ids := make([]string, 0, len(l.Data))
	for _, m := range l.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
