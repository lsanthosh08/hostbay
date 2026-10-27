package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// anthropicVersion is the Messages API version Hostbay speaks.
const anthropicVersion = "2026-10-21"

// maxTokens caps one reply.
const maxTokens = 32000

// anthropic is a Messages API client, streaming with tool calls.
type anthropic struct {
	base, key, ua string
	c             *http.Client
}

func (a *anthropic) header() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("anthropic-version", anthropicVersion)
	h.Set("User-Agent", a.ua)
	if a.key != "" {
		h.Set("x-api-key", a.key)
	}
	return h
}

type aTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type aMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

func (a *anthropic) body(r Request) ([]byte, error) {
	msgs := make([]aMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		out := aMessage{Role: m.Role}
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				out.Content = append(out.Content, map[string]any{"type": "text", "text": b.Text})
			case "tool_use":
				out.Content = append(out.Content, map[string]any{"type": "tool_use", "id": b.ToolID, "name": b.Name, "input": b.Input})
			case "tool_result":
				out.Content = append(out.Content, map[string]any{"type": "tool_result", "tool_use_id": b.ToolID, "content": b.Text, "is_error": b.IsError})
			case "opaque":
				out.Content = append(out.Content, b.Opaque)
			}
		}
		msgs = append(msgs, out)
	}
	tools := make([]aTool, 0, len(r.Tools))
	for _, t := range r.Tools {
		tools = append(tools, aTool{t.Name, t.Description, t.Schema})
	}
	body := map[string]any{"model": r.Model, "max_tokens": maxTokens, "stream": true, "system": r.System, "messages": msgs}
	if len(tools) > 0 { // a one-shot request offers none
		body["tools"] = tools
	}
	return json.Marshal(body)
}

// aBlock collects one content block while it streams.
type aBlock struct {
	typ       string
	start     json.RawMessage
	text      []byte
	id, name  string
	input     []byte // input_json_delta pieces, parsed at content_block_stop
	thinking  []byte
	signature []byte
	done      *Block
}

func (a *anthropic) Stream(ctx context.Context, r Request, text func(string)) (Reply, error) {
	body, err := a.body(r)
	if err != nil {
		return Reply{}, err
	}
	rd, done, err := do(ctx, a.c, "POST", a.base+"/v1/messages", a.header(), body)
	if err != nil {
		return Reply{}, err
	}
	defer done()
	blocks := map[int]*aBlock{}
	var order []int
	var stop string
	var finished bool
	err = readSSE(rd, func(_, data string) (bool, error) {
		var ev struct {
			Type         string          `json:"type"`
			Index        int             `json:"index"`
			ContentBlock json.RawMessage `json:"content_block"`
			Delta        json.RawMessage `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return false, fmt.Errorf("the provider sent an event Hostbay cannot read: %w", err)
		}
		switch ev.Type {
		case "content_block_start":
			var cb struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(ev.ContentBlock, &cb)
			blocks[ev.Index] = &aBlock{typ: cb.Type, start: ev.ContentBlock, id: cb.ID, name: cb.Name, text: []byte(cb.Text)}
			order = append(order, ev.Index)
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil {
				return false, errors.New("the provider sent a delta for a block it never started")
			}
			var d struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
			}
			_ = json.Unmarshal(ev.Delta, &d)
			switch d.Type {
			case "text_delta":
				b.text = append(b.text, d.Text...)
				text(d.Text)
			case "input_json_delta":
				b.input = append(b.input, d.PartialJSON...)
			case "thinking_delta":
				b.thinking = append(b.thinking, d.Thinking...)
			case "signature_delta":
				b.signature = append(b.signature, d.Signature...)
			}
		case "content_block_stop":
			b := blocks[ev.Index]
			if b == nil {
				return false, errors.New("the provider ended a block it never started")
			}
			out, err := b.finish()
			if err != nil {
				return false, err
			}
			b.done = out
		case "message_delta":
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(ev.Delta, &d)
			if d.StopReason != "" {
				stop = d.StopReason
			}
		case "message_stop":
			finished = true
			return true, nil
		case "error":
			typ, msg := errorFields([]byte(data))
			return false, &StatusError{Status: http.StatusOK, Type: typ, Message: msg}
		}
		return false, nil // ping and events this build does not know
	})
	if !finished {
		return Reply{}, streamErr(err)
	}
	msg := Message{Role: "assistant"}
	for _, i := range order {
		b := blocks[i]
		if b.done == nil {
			return Reply{}, errEarlyEnd
		}
		if b.done.Type != "" {
			msg.Content = append(msg.Content, *b.done)
		}
	}
	return Reply{Message: msg, StopReason: stop}, nil
}

// finish turns a streamed block into a Block. Thinking blocks are kept to
// be sent back unchanged; blocks of a kind Hostbay does not use are dropped.
func (b *aBlock) finish() (*Block, error) {
	switch b.typ {
	case "text":
		return &Block{Type: "text", Text: string(b.text)}, nil
	case "tool_use":
		in := b.input
		if len(in) == 0 {
			in = []byte("{}")
		}
		if !json.Valid(in) {
			return nil, fmt.Errorf("the model's call to %s is not valid JSON", b.name)
		}
		return &Block{Type: "tool_use", ToolID: b.id, Name: b.name, Input: json.RawMessage(in)}, nil
	case "thinking":
		raw, _ := json.Marshal(map[string]string{"type": "thinking", "thinking": string(b.thinking), "signature": string(b.signature)})
		return &Block{Type: "opaque", Opaque: raw}, nil
	case "redacted_thinking":
		return &Block{Type: "opaque", Opaque: b.start}, nil
	}
	return &Block{}, nil
}

func (a *anthropic) Models(ctx context.Context) ([]string, error) {
	rd, done, err := do(ctx, a.c, "GET", a.base+"/v1/models?limit=100", a.header(), nil)
	if err != nil {
		return nil, err
	}
	defer done()
	return modelIDs(rd)
}
