package tunnel

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"hostbay/internal/netd"
)

// Windows: OpenConnect starts on the Wintun adapter the helper created,
// named by --interface, with the password on stdin and the install's cscript
// wrapper, a bare path openconnect quotes itself, as its script; it is up once
// openconnect says it is configured, and the adapter goes with it.
func TestOpenConnectRunsOnTheHelpersAdapter(t *testing.T) {
	e := newEnv(t)
	var made []string
	closed := make(chan struct{})
	e.m.adapter = func(name string) (io.Closer, error) {
		made = append(made, name)
		return closer(func() { close(closed) }), nil
	}
	e.run = func(s started, stdout io.Writer, _ *fakeProc) {
		go fmt.Fprintln(stdout, "Configured as 10.1.2.3, with SSL connected and DTLS in progress")
	}
	r, err := e.start(netd.Msg{T: netd.OpenConnectStart, OpenConnect: &netd.OpenConnect{Type: "anyconnect", Config: `{"gateway":"vpn.example.test","user":"alice"}`, Password: "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	const name = "Hostbay AB12CD34"
	s := <-e.starts
	if r.Iface != name || !slices.Equal(made, []string{name}) || !slices.Contains(s.args, "--interface="+name) || s.stdin != "pw\n" ||
		!slices.Contains(s.args, "--script="+filepath.Join(e.dir, "oc-script.js")) ||
		slices.ContainsFunc(s.args, func(a string) bool { return strings.Contains(a, "pw") && !strings.Contains(a, "passwd") }) {
		t.Fatalf("reply %+v, adapters %v, args %q, stdin %q", r, made, s.args, s.stdin)
	}
	if ev := e.event(t); ev.State != netd.TunnelUp || ev.Iface != name || !slices.Equal(ev.Address, []string{"10.1.2.3"}) {
		t.Fatalf("event %+v", ev)
	}
	e.m.Stop(handle)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the adapter outlived its tunnel")
	}
}

type closer func()

func (c closer) Close() error { c(); return nil }
