package openvpn

import (
	"bufio"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeOpenVPN is the openvpn end of a management connection: it writes what
// a test tells it and records every command Hostbay sends.
type fakeOpenVPN struct {
	conn net.Conn
	cmds chan string
}

func manage(t *testing.T, user, pass, mgmtPass string) (*fakeOpenVPN, <-chan Event, <-chan error) {
	t.Helper()
	a, b := net.Pipe()
	f := &fakeOpenVPN{conn: b, cmds: make(chan string, 16)}
	go func() {
		sc := bufio.NewScanner(b)
		for sc.Scan() {
			f.cmds <- sc.Text()
		}
		close(f.cmds)
	}()
	events := make(chan Event, 16)
	done := make(chan error, 1)
	go func() { done <- Manage(a, user, pass, mgmtPass, func(e Event) { events <- e }) }()
	t.Cleanup(func() { b.Close() })
	return f, events, done
}

func (f *fakeOpenVPN) say(t *testing.T, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if _, err := f.conn.Write([]byte(l + "\r\n")); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fakeOpenVPN) expect(t *testing.T, want ...string) {
	t.Helper()
	for _, w := range want {
		select {
		case got := <-f.cmds:
			if got != w {
				t.Fatalf("command %q, want %q", got, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no command, want %q", w)
		}
	}
}

func next(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case e := <-events:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

func TestManageReleasesHoldSendsCredentialsOnceAndReportsConnected(t *testing.T) {
	f, events, done := manage(t, `al"ice`, `p\ss word`, "")
	f.expect(t, "state on")
	f.say(t, ">INFO:OpenVPN Management Interface Version 5 -- type 'help' for more info", ">HOLD:Waiting for hold release:0")
	f.expect(t, "hold release")
	f.say(t, ">STATE:1700000000,RESOLVE,,,,,,", ">PASSWORD:Need 'Auth' username/password")
	f.expect(t, `username "Auth" "al\"ice"`, `password "Auth" "p\\ss word"`)
	if e := next(t, events); e.State != Connecting {
		t.Fatalf("event %+v, want connecting", e)
	}
	f.say(t, ">STATE:1700000001,CONNECTED,SUCCESS,10.8.0.6,203.0.113.9,1194,,")
	if e := next(t, events); e.State != Connected || e.Local != "10.8.0.6" {
		t.Fatalf("event %+v, want connected at 10.8.0.6", e)
	}
	// A second ask is never answered: the credentials go once.
	f.say(t, ">PASSWORD:Need 'Auth' username/password")
	f.expect(t, "signal SIGTERM")
	if e := next(t, events); e.State != Failed {
		t.Fatalf("event %+v, want failed", e)
	}
	f.conn.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestManageReportsARefusedPassword(t *testing.T) {
	f, events, _ := manage(t, "alice", "wrong", "")
	f.expect(t, "state on")
	f.say(t, ">PASSWORD:Need 'Auth' username/password")
	f.expect(t, `username "Auth" "alice"`, `password "Auth" "wrong"`)
	f.say(t, ">PASSWORD:Verification Failed: 'Auth'")
	if e := next(t, events); e.State != Failed || !strings.Contains(e.Reason, "refused") {
		t.Fatalf("event %+v", e)
	}
}

// On Windows the interface asks for its password first, without a line end.
func TestManageWindowsPassword(t *testing.T) {
	f, events, _ := manage(t, "", "", "s3cret")
	f.expect(t, "s3cret", "state on")
	f.say(t, "ENTER PASSWORD:SUCCESS: password is correct", ">STATE:1,EXITING,SIGTERM,,,,,")
	if e := next(t, events); e.State != Exiting {
		t.Fatalf("event %+v", e)
	}
}

// A line break in a credential would add a management command of its own.
func TestManageRefusesLineBreaksInCredentials(t *testing.T) {
	for _, c := range [][2]string{{"alice\nsignal SIGTERM", "pw"}, {"alice", "pw\rhold release"}} {
		a, b := net.Pipe()
		go func() { buf := make([]byte, 64); b.Read(buf) }()
		if err := Manage(a, c[0], c[1], "", func(Event) {}); err == nil {
			t.Errorf("%q accepted", c)
		}
		a.Close()
		b.Close()
	}
}

func TestEventStates(t *testing.T) {
	got := []string{}
	for _, l := range []string{"WAIT", "AUTH", "GET_CONFIG", "ASSIGN_IP", "ADD_ROUTES", "RECONNECTING", "TCP_CONNECT"} {
		got = append(got, stateEvent(strings.Split("1,"+l+",x,,,", ",")).State)
	}
	if slices.ContainsFunc(got, func(s string) bool { return s != Connecting }) {
		t.Fatalf("states %v", got)
	}
}
