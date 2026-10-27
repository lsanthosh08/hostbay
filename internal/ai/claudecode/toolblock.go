package claudecode

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"hostbay/internal/ai"
)

// newTag returns the random tag one Stream call uses to mark tool blocks.
// The model is told the tag in the prompt, so text that reaches it from a
// terminal cannot contain the right one (KTD4).
func newTag() string { return randHex("hb-", 6) }

func newToolID() string { return randHex("cc_", 9) }

func randHex(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system has no randomness
	}
	return prefix + hex.EncodeToString(b)
}

// blockParser splits a reply into visible text and tool calls. A call is
// <tag>{"name":...,"input":{...}}</tag>; text is passed on as soon as it can
// no longer turn out to be the start of that tag, so a tag split across
// chunks never reaches the page half-formed.
type blockParser struct {
	open, close string
	known       map[string]bool // tool names the request offers
	keepBad     bool            // an unreadable block becomes text (the second try)
	emit        func(string)    // visible text, as it is settled

	held    string // text not yet settled: may be the start of open
	inBlock bool
	block   strings.Builder
	visible strings.Builder // everything emitted
	calls   []ai.Block
	bad     int // blocks that could not be read
}

func newBlockParser(tag string, tools []ai.Tool, keepBad bool, emit func(string)) *blockParser {
	known := map[string]bool{}
	for _, t := range tools {
		known[t.Name] = true
	}
	return &blockParser{open: "<" + tag + ">", close: "</" + tag + ">", known: known, keepBad: keepBad, emit: emit}
}

func (p *blockParser) write(s string) {
	p.held += s
	for {
		if p.inBlock {
			i := strings.Index(p.held, p.close)
			if i < 0 { // all but a tail that could still become the closing tag
				keep := partialSuffix(p.held, p.close)
				p.block.WriteString(p.held[:len(p.held)-keep])
				p.held = p.held[len(p.held)-keep:]
				return
			}
			p.block.WriteString(p.held[:i])
			p.held = p.held[i+len(p.close):]
			p.inBlock = false
			p.finishBlock()
			continue
		}
		i := strings.Index(p.held, p.open)
		if i >= 0 {
			p.say(p.held[:i])
			p.held = p.held[i+len(p.open):]
			p.inBlock = true
			p.block.Reset()
			continue
		}
		// No tag yet: everything but a tail that could still become one.
		keep := partialSuffix(p.held, p.open)
		p.say(p.held[:len(p.held)-keep])
		p.held = p.held[len(p.held)-keep:]
		return
	}
}

// flush ends the reply. Text still held is plain text; a block that never
// closed is an unreadable one.
func (p *blockParser) flush() {
	if p.inBlock {
		p.block.WriteString(p.held)
		p.held, p.inBlock = "", false
		p.bad++
		if p.keepBad {
			p.say(p.open + p.block.String())
		}
		return
	}
	p.say(p.held)
	p.held = ""
}

func (p *blockParser) say(s string) {
	if s == "" {
		return
	}
	p.visible.WriteString(s)
	p.emit(s)
}

func (p *blockParser) finishBlock() {
	raw := p.block.String()
	var c struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	in := strings.TrimSpace(raw)
	if json.Unmarshal([]byte(in), &c) == nil && p.known[c.Name] && len(c.Input) > 0 && c.Input[0] == '{' {
		p.calls = append(p.calls, ai.Block{Type: "tool_use", ToolID: newToolID(), Name: c.Name, Input: c.Input})
		return
	}
	p.bad++
	if p.keepBad {
		p.say(p.open + raw + p.close)
	}
}

// partialSuffix is the length of the longest suffix of s that is a proper
// prefix of tag.
func partialSuffix(s, tag string) int {
	for n := min(len(s), len(tag)-1); n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}
