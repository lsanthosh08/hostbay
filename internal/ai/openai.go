package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// openAI is an OpenAI-compatible Chat Completions client, streaming with
// tool calls: OpenAI itself, Ollama, LM Studio, vLLM and OpenRouter.
type openAI struct {
	base, key, ua string
	c             *http.Client
}

func (o *openAI) header() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", o.ua)
	if o.key != "" {
		h.Set("Authorization", "Bearer "+o.key)
	}
	return h
}

type oCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (o *openAI) body(r Request) ([]byte, error) {
	msgs := []map[string]any{{"role": "system", "content": r.System}}
	for _, m := range r.Messages {
		var text []string
		var calls []oCall
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				text = append(text, b.Text)
			case "tool_use":
				c := oCall{ID: b.ToolID, Type: "function"}
				c.Function.Name, c.Function.Arguments = b.Name, string(b.Input)
				calls = append(calls, c)
			case "tool_result":
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": b.ToolID, "content": b.Text})
			}
		}
		if len(text) == 0 && len(calls) == 0 {
			continue
		}
		out := map[string]any{"role": m.Role, "content": strings.Join(text, "\n")}
		if len(calls) > 0 {
			out["tool_calls"] = calls
		}
		msgs = append(msgs, out)
	}
	tools := make([]map[string]any, 0, len(r.Tools))
	for _, t := range r.Tools {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": t.Schema,
		}})
	}
	body := map[string]any{"model": r.Model, "stream": true, "messages": msgs}
	if len(tools) > 0 { // OpenAI refuses an empty tools list
		body["tools"] = tools
	}
	return json.Marshal(body)
}

func (o *openAI) Stream(ctx context.Context, r Request, text func(string)) (Reply, error) {
	body, err := o.body(r)
	if err != nil {
		return Reply{}, err
	}
	rd, done, err := do(ctx, o.c, "POST", o.base+"/chat/completions", o.header(), body)
	if err != nil {
		return Reply{}, err
	}
	defer done()
	var reply strings.Builder
	var calls []*oCall // fragments joined by index
	var stop string
	err = readSSE(rd, func(_, data string) (bool, error) {
		if strings.TrimSpace(data) == "[DONE]" {
			return true, nil
		}
		var ch struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content   *string `json:"content"`
					ToolCalls []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return false, fmt.Errorf("the provider sent an event Hostbay cannot read: %w", err)
		}
		if len(ch.Error) > 0 && string(ch.Error) != "null" {
			typ, msg := errorFields([]byte(data))
			return false, &StatusError{Status: http.StatusOK, Type: typ, Message: msg}
		}
		for _, c := range ch.Choices {
			if c.Delta.Content != nil {
				reply.WriteString(*c.Delta.Content)
				text(*c.Delta.Content)
			}
			for _, tc := range c.Delta.ToolCalls {
				// A server that sends no index (some Ollama builds) sends each
				// call whole: a new ID starts a new call.
				i := len(calls) - 1
				switch {
				case tc.Index != nil:
					i = *tc.Index
				case tc.ID != "" && (i < 0 || calls[i].ID != tc.ID):
					i = len(calls)
				}
				if i < 0 || i > len(calls) || i > 64 {
					return false, fmt.Errorf("the provider sent tool call %d out of order", i)
				}
				if i == len(calls) {
					calls = append(calls, &oCall{})
				}
				if tc.ID != "" {
					calls[i].ID = tc.ID
				}
				calls[i].Function.Name += tc.Function.Name
				calls[i].Function.Arguments += tc.Function.Arguments
			}
			if c.FinishReason != nil && *c.FinishReason != "" {
				stop = *c.FinishReason
			}
		}
		return false, nil
	})
	if stop == "" { // no finish_reason: the reply may be cut short
		return Reply{}, streamErr(err)
	}
	if err != nil && streamErr(err) != errEarlyEnd {
		return Reply{}, err
	}
	msg := Message{Role: "assistant"}
	if reply.Len() > 0 {
		msg.Content = append(msg.Content, Block{Type: "text", Text: reply.String()})
	}
	for n, c := range calls {
		args := strings.TrimSpace(c.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return Reply{}, fmt.Errorf("the model's call to %s is not valid JSON", c.Function.Name)
		}
		if c.ID == "" {
			c.ID = fmt.Sprintf("call_%d", n)
		}
		msg.Content = append(msg.Content, Block{Type: "tool_use", ToolID: c.ID, Name: c.Function.Name, Input: json.RawMessage(args)})
	}
	return Reply{Message: msg, StopReason: stop}, nil
}

func (o *openAI) Models(ctx context.Context) ([]string, error) {
	rd, done, err := do(ctx, o.c, "GET", o.base+"/models", o.header(), nil)
	if err != nil {
		return nil, err
	}
	defer done()
	return modelIDs(rd)
}
