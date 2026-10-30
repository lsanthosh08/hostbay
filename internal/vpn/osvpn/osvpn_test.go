package osvpn

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// fake answers each command with out and records its argv.
type fake struct {
	argv [][]string
	out  string
	err  error
}

func use(t *testing.T, goos string, f *fake) {
	t.Helper()
	oldRun, oldOS := run, platform
	run = func(_ context.Context, name string, args ...string) (string, error) {
		f.argv = append(f.argv, append([]string{name}, args...))
		return f.out, f.err
	}
	platform = goos
	t.Cleanup(func() { run, platform = oldRun, oldOS })
}

func TestCommands(t *testing.T) {
	ctx := context.Background()
	for goos, want := range map[string][2][]string{
		"darwin":  {{"/usr/sbin/scutil", "--nc", "start", "Office VPN"}, {"/usr/sbin/scutil", "--nc", "stop", "Office VPN"}},
		"windows": {{"rasdial.exe", "Office VPN"}, {"rasdial.exe", "Office VPN", "/disconnect"}},
		"linux":   {{"nmcli", "connection", "up", "id", "Office VPN"}, {"nmcli", "connection", "down", "id", "Office VPN"}},
	} {
		f := &fake{}
		use(t, goos, f)
		if err := Connect(ctx, "Office VPN"); err != nil {
			t.Fatal(err)
		}
		if err := Disconnect(ctx, "Office VPN"); err != nil {
			t.Fatal(err)
		}
		if len(f.argv) != 2 || !slices.Equal(f.argv[0], want[0]) || !slices.Equal(f.argv[1], want[1]) {
			t.Errorf("%s: argv %q, want %q", goos, f.argv, want)
		}
	}
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		goos, out string
		want      Status
	}{
		{"darwin", "Connected\nExtended Status <dictionary> {\n  IPv4 : <dictionary> {\n    Addresses : <array> {\n      0 : 10.0.0.2\n    }\n    InterfaceName : ipsec0\n  }\n}\n", Status{Connected: true, Iface: "ipsec0"}},
		{"darwin", "Disconnected\nExtended Status <dictionary> {\n}\n", Status{}},
		{"darwin", "Connecting\n", Status{}},
		// rasdial's words are localized: only the entry's own line counts.
		{"windows", "Verbunden mit\r\nOffice VPN\r\nDer Befehl wurde erfolgreich ausgeführt.\r\n", Status{Connected: true, Iface: "Office VPN"}},
		{"windows", "Connected to\r\nOffice VPN 2\r\nCommand completed successfully.\r\n", Status{}},
		{"windows", "No connections\r\nCommand completed successfully.\r\n", Status{}},
		{"linux", "GENERAL.STATE:activated\nGENERAL.IP-IFACE:ppp0\n", Status{Connected: true, Iface: "ppp0"}},
		{"linux", "GENERAL.STATE:activating\nGENERAL.IP-IFACE:\n", Status{}},
		{"linux", "", Status{}},
	} {
		f := &fake{out: c.out}
		use(t, c.goos, f)
		got, err := State(ctx, "Office VPN")
		if err != nil || got != c.want {
			t.Errorf("%s %q: %+v %v, want %+v", c.goos, c.out, got, err, c.want)
		}
	}
	f := &fake{}
	use(t, "linux", f)
	State(ctx, "Office VPN")
	if want := []string{"nmcli", "-t", "-f", "GENERAL.STATE,GENERAL.IP-IFACE", "connection", "show", "id", "Office VPN"}; !slices.Equal(f.argv[0], want) {
		t.Errorf("linux status argv %q", f.argv[0])
	}
}

// A name is one argument, never an option, and a failed command says what
// the system said.
func TestNamesAndFailures(t *testing.T) {
	f := &fake{}
	use(t, "darwin", f)
	for _, n := range []string{"", "-x", "/etc/x", "a\nb", "a\x00b"} {
		if err := Connect(context.Background(), n); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
	if len(f.argv) != 0 {
		t.Fatalf("ran %q", f.argv)
	}
	f.out, f.err = "No service\n", errors.New("exit status 1")
	if err := Connect(context.Background(), "Office"); err == nil || err.Error() != "Office: No service" {
		t.Errorf("err %v", err)
	}
	use(t, "plan9", &fake{})
	if err := Connect(context.Background(), "Office"); err == nil {
		t.Error("an unknown system connected")
	}
}
