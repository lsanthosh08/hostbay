package tunnel

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hostbay/internal/netd"
)

// macOS: OpenConnect starts on a utun the helper picked, named by
// --interface and as the argument of the helper's own script, with the
// password on stdin; it is up once openconnect says it is configured. A
// profile with no pinned certificate trusts macOS's root-owned CA bundle,
// never the one compiled into openconnect's libraries.
func TestOpenConnectRunsOnTheHelpersUtun(t *testing.T) {
	pin := "pin-sha256:" + strings.Repeat("A", 43) + "="
	for _, c := range []struct {
		conf     string
		ownTrust bool
	}{
		{`{"gateway":"vpn.example.test","user":"alice"}`, true},
		{`{"gateway":"vpn.example.test","user":"alice","serverCert":"` + pin + `"}`, false},
	} {
		e := newEnv(t)
		e.run = func(s started, stdout io.Writer, _ *fakeProc) {
			go fmt.Fprintln(stdout, "Configured as 10.0.0.5, with SSL connected and DTLS in progress")
		}
		r, err := e.start(netd.Msg{T: netd.OpenConnectStart, OpenConnect: &netd.OpenConnect{Type: "sslvpn", Config: c.conf, Password: "pw"}})
		if err != nil {
			t.Fatal(err)
		}
		s := <-e.starts
		script := "--script='" + filepath.Join(e.dir, "hostbay-netd") + "' oc-script " + r.Iface
		if !utunName.MatchString(r.Iface) || !slices.Contains(s.args, "--interface="+r.Iface) || !slices.Contains(s.args, script) || s.stdin != "pw\n" ||
			slices.ContainsFunc(s.args, func(a string) bool { return strings.Contains(a, "pw") && !strings.Contains(a, "passwd") }) {
			t.Fatalf("reply %+v, args %q, stdin %q", r, s.args, s.stdin)
		}
		trust := slices.Contains(s.args, "--no-system-trust") && slices.Contains(s.args, "--cafile=/etc/ssl/cert.pem")
		if trust != c.ownTrust || !c.ownTrust && slices.ContainsFunc(s.args, func(a string) bool { return strings.HasPrefix(a, "--cafile") || a == "--no-system-trust" }) {
			t.Errorf("%s: args %q, want the system CA bundle %v", c.conf, s.args, c.ownTrust)
		}
		if ev := e.event(t); ev.State != netd.TunnelUp || ev.Iface != r.Iface || !slices.Equal(ev.Address, []string{"10.0.0.5"}) {
			t.Fatalf("event %+v", ev)
		}
		e.m.Stop(handle)
		if ev := e.event(t); ev.State != netd.TunnelEnded {
			t.Fatalf("event %+v", ev)
		}
	}
}

// Two tunnels started while the first is still signing in, before its
// utun exists, get different utuns; an ended tunnel's utun is free again.
func TestOpenConnectTunnelsStartingTogetherGetTheirOwnUtun(t *testing.T) {
	e := newEnv(t)
	start := func(h string) string {
		t.Helper()
		r, err := e.m.Start(netd.Msg{T: netd.OpenConnectStart, Tunnel: h,
			OpenConnect: &netd.OpenConnect{Type: "sslvpn", Config: `{"gateway":"vpn.example.test","user":"alice"}`, Password: "pw"}},
			func(ev netd.Msg) { e.events <- ev })
		if err != nil {
			t.Fatal(err)
		}
		if s := <-e.starts; !slices.Contains(s.args, "--interface="+r.Iface) {
			t.Fatalf("args %q, want --interface=%s", s.args, r.Iface)
		}
		return r.Iface
	}
	a, b := start("AAAAAAAAAA"), start("BBBBBBBBBB")
	if a == b {
		t.Fatalf("both tunnels got %s", a)
	}
	e.m.Stop("AAAAAAAAAA")
	if c := start("CCCCCCCCCC"); c != a {
		t.Errorf("after %s ended the next tunnel got %s, want %s again", a, c, a)
	}
}
