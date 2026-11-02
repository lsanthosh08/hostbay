package app

import (
	"strings"
	"testing"

	"hostbay/internal/agentexec"
)

type fakeNotice struct {
	sent    []string
	actions map[string]bool
	cleared []string
}

func (f *fakeNotice) Send(id, _, _ string, actions bool) error {
	f.sent = append(f.sent, id)
	if f.actions == nil {
		f.actions = map[string]bool{}
	}
	f.actions[id] = actions
	return nil
}
func (f *fakeNotice) Clear(id string) { f.cleared = append(f.cleared, id) }

type noticeRig struct {
	m       *MCPService
	n       *fakeNotice
	raised  int
	answers map[string]bool
}

func newNoticeRig() *noticeRig {
	r := &noticeRig{n: &fakeNotice{}, answers: map[string]bool{}}
	r.m = &MCPService{seen: map[string]bool{}, offered: map[string]bool{}, notice: r.n,
		Raise: func() { r.raised++ },
		answer: func(id string, allow bool) error {
			r.answers[id] = allow
			return nil
		}}
	return r
}

func mcpPending(id, cmd string) agentexec.Pending {
	return agentexec.Pending{ID: id, Principal: "mcp:t1", Client: "Claude", Host: "web1", Tool: "run_command", Bytes: cmd}
}

// Each new MCP request raises the window and notifies once; one that
// leaves the tray withdraws its notification; the agent's own do nothing.
func TestMCPRaiseAndNotifyEachRequestOnce(t *testing.T) {
	r := newNoticeRig()
	a, b := mcpPending("a", "systemctl restart nginx"), mcpPending("b", "reboot")
	r.m.raise([]agentexec.Pending{a, {ID: "x", Principal: "conv1", Tool: "run_command"}})
	r.m.raise([]agentexec.Pending{a})
	if r.raised != 1 || len(r.n.sent) != 1 {
		t.Fatalf("raised %d, sent %v", r.raised, r.n.sent)
	}
	r.m.raise([]agentexec.Pending{b}) // a answered as b arrived
	if r.raised != 2 || len(r.n.sent) != 2 || len(r.n.cleared) != 1 || r.n.cleared[0] != "a" {
		t.Fatalf("raised %d, sent %v, cleared %v", r.raised, r.n.sent, r.n.cleared)
	}
	r.m.raise(nil)
	if len(r.n.cleared) != 2 || r.raised != 2 {
		t.Fatalf("raised %d, cleared %v", r.raised, r.n.cleared)
	}
}

// Allow and Deny answer only a request whose whole text the notification
// showed; any other click, or a click on a long one, opens the window.
func TestMCPNoticeAnswers(t *testing.T) {
	r := newNoticeRig()
	long := mcpPending("long", "echo "+strings.Repeat("x", 300))
	multi := mcpPending("multi", "ls\nrm -rf /")
	short := mcpPending("short", "systemctl restart nginx")
	r.m.raise([]agentexec.Pending{long, multi, short})
	if r.n.actions["long"] || r.n.actions["multi"] || !r.n.actions["short"] {
		t.Fatalf("actions = %v", r.n.actions)
	}
	before := r.raised
	r.m.noticeAnswered("long", noticeAllow)
	r.m.noticeAnswered("multi", noticeAllow)
	r.m.noticeAnswered("nope", noticeAllow)
	r.m.noticeAnswered("short", "")
	if len(r.answers) != 0 || r.raised != before+4 {
		t.Fatalf("answers %v, raised %d", r.answers, r.raised-before)
	}
	r.m.noticeAnswered("short", noticeDeny)
	if v, ok := r.answers["short"]; !ok || v {
		t.Fatalf("deny = %v", r.answers)
	}
	r.m.noticeAnswered("short", noticeAllow)
	if !r.answers["short"] {
		t.Fatalf("allow = %v", r.answers)
	}
}

func TestNoticeTextVPN(t *testing.T) {
	title, body, actions := noticeText(agentexec.Pending{Client: "Claude", Host: "Office", Tool: "connect_vpn", Tier: "system", Gateway: "vpn.example.com", Tag: "acme"})
	if !strings.Contains(title, "Claude") || body != "connect VPN Office, tier system, gateway vpn.example.com, tag acme" || !actions {
		t.Fatalf("%q %q %v", title, body, actions)
	}
}

// The text at the limit keeps Allow and Deny; one rune over loses them.
func TestNoticeTextLimit(t *testing.T) {
	p := agentexec.Pending{Host: "h", Tool: "run_command"}
	p.Bytes = strings.Repeat("x", noticeMax-len("h: "))
	if _, _, actions := noticeText(p); !actions {
		t.Fatal("a request that fits lost its buttons")
	}
	p.Bytes += "x"
	if _, body, actions := noticeText(p); actions || !strings.HasSuffix(body, "…") {
		t.Fatalf("a request that does not fit kept its buttons: %q", body)
	}
}
