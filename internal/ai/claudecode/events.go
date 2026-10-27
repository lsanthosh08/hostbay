package claudecode

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const (
	maxLine    = 4 << 20 // an init event carries every plugin name
	maxRetries = 2       // api_retry events before the call is ended
)

type resultEvent struct {
	IsError        bool
	APIErrorStatus int
}

// streamState is what a call's events add up to.
type streamState struct {
	version   string
	errCode   string // the error field of an assistant event
	result    *resultEvent
	gotDelta  bool
	finalText string
	retries   int
}

// readStream reads stream-json events until the program closes its output.
// onText gets each text delta. It stops early, with an error, on anything
// that shows Claude Code is configured to run tools or hooks (KTD2): the
// flags turn them off, so only admin policy can bring them back.
func readStream(r io.Reader, s *streamState, onText func(string), onActivity func()) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			onActivity()
			if err := s.event(line, onText); err != nil {
				return err
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			return buf, err
		}
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, errChanged
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

func (s *streamState) event(line []byte, onText func(string)) error {
	var e struct {
		Type           string            `json:"type"`
		Subtype        string            `json:"subtype"`
		Error          string            `json:"error"`
		Version        string            `json:"claude_code_version"`
		Tools          []json.RawMessage `json:"tools"`
		MCP            []json.RawMessage `json:"mcp_servers"`
		Skills         []json.RawMessage `json:"skills"`
		Commands       []json.RawMessage `json:"slash_commands"`
		Event          json.RawMessage   `json:"event"`
		Message        json.RawMessage   `json:"message"`
		IsError        bool              `json:"is_error"`
		APIErrorStatus *int              `json:"api_error_status"`
	}
	if json.Unmarshal(line, &e) != nil {
		return nil // not an event; nothing in it is used
	}
	switch e.Type {
	case "system":
		switch e.Subtype {
		case "init":
			s.version = e.Version
			if len(e.Tools)+len(e.MCP)+len(e.Skills)+len(e.Commands) > 0 {
				return errConfigured
			}
		case "hook_started", "hook_response", "hook_progress":
			return errConfigured
		case "api_retry":
			if s.retries++; s.retries >= maxRetries {
				return errNetwork
			}
		}
	case "stream_event":
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(e.Event, &ev) == nil && ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
			s.gotDelta = true
			onText(ev.Delta.Text)
		}
	case "assistant":
		if e.Error != "" {
			s.errCode = e.Error
		}
		var m struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(e.Message, &m) == nil {
			var b strings.Builder
			for _, c := range m.Content {
				if c.Type == "text" {
					b.WriteString(c.Text)
				}
			}
			s.finalText = b.String()
		}
	case "result":
		r := &resultEvent{IsError: e.IsError}
		if e.APIErrorStatus != nil {
			r.APIErrorStatus = *e.APIErrorStatus
		}
		s.result = r
	}
	return nil
}
