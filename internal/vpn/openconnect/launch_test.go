package openconnect

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"hostbay/internal/vpn/proto"
)

const (
	testPass = "Hunter2-VPN!"
	pinA     = "pin-sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	pinB     = "pin-sha256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="
	self     = "/opt/Host Bay/it's/hostbay-vpn"
)

func TestArgsNeverCarryThePassword(t *testing.T) {
	want := map[string]string{
		proto.TypeAnyConnect:     "anyconnect",
		proto.TypeSSLVPN:         "fortinet",
		proto.TypeGlobalProtect:  "gp",
		proto.TypePulse:          "pulse",
		proto.TypeNetworkConnect: "nc",
		proto.TypeF5:             "f5",
		proto.TypeArray:          "array",
	}
	for typ, protocol := range want {
		l, err := prepare(typ, `{"gateway":"vpn.example.test:8443","user":"alice","serverCert":"`+pinA+`"}`,
			map[string]string{"password": testPass})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		args := l.args(self)
		for _, a := range args {
			if strings.Contains(a, testPass) {
				t.Fatalf("%s: argv carries the password: %q", typ, args)
			}
		}
		for _, w := range []string{"--protocol=" + protocol, "--non-inter", "--passwd-on-stdin", "--script-tun",
			`--script='/opt/Host Bay/it'\''s/hostbay-vpn' tun-fd`, "--user=alice", "--servercert=" + pinA} {
			if !slices.Contains(args, w) {
				t.Errorf("%s: argv %q lacks %q", typ, args, w)
			}
		}
		if n := len(args); args[n-2] != "--" || args[n-1] != "vpn.example.test:8443" {
			t.Errorf("%s: argv %q does not end with -- and the gateway", typ, args)
		}
	}
	if len(proto.OpenConnectTypes) != len(want) {
		t.Errorf("OpenConnectTypes has %d types, the test knows %d", len(proto.OpenConnectTypes), len(want))
	}
}

func TestBrowserSignIn(t *testing.T) {
	l, err := prepare(proto.TypeAnyConnect, `{"gateway":"vpn.example.test","browser":true}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	args := l.args(self)
	if !slices.Contains(args, "--external-browser="+self) || slices.Contains(args, "--passwd-on-stdin") || !slices.Contains(args, "--non-inter") {
		t.Fatalf("argv %q", args)
	}
	// R20: a protocol without browser sign-in does not offer it.
	for typ := range proto.OpenConnectTypes {
		if typ == proto.TypeAnyConnect {
			continue
		}
		if _, err := prepare(typ, `{"gateway":"vpn.example.test","browser":true}`, nil); err == nil {
			t.Errorf("%s accepted browser sign-in", typ)
		}
	}
}

func TestAuthGroup(t *testing.T) {
	l, err := prepare(proto.TypeAnyConnect, `{"gateway":"vpn.example.test","browser":true,"authGroup":"Employee"}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if args := l.args(self); !slices.Contains(args, "--authgroup=Employee") {
		t.Fatalf("argv %q", args)
	}
	l, _ = prepare(proto.TypeAnyConnect, `{"gateway":"vpn.example.test"}`, nil)
	for _, a := range l.args(self) {
		if strings.HasPrefix(a, "--authgroup") {
			t.Fatalf("no group set, got %q", a)
		}
	}
}

func TestPrepareRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		typ, conf string
		secrets   map[string]string
	}{
		"unknown type":           {"carrier-pigeon", `{"gateway":"vpn.example.test"}`, nil},
		"no gateway":             {proto.TypeAnyConnect, `{}`, nil},
		"gateway as an option":   {proto.TypeAnyConnect, `{"gateway":"--script=/tmp/x"}`, nil},
		"gateway with a space":   {proto.TypeAnyConnect, `{"gateway":"vpn.example.test --script=x"}`, nil},
		"user with a line break": {proto.TypeAnyConnect, `{"gateway":"vpn.example.test","user":"a\nb"}`, nil},
		"group as an option":     {proto.TypeAnyConnect, `{"gateway":"vpn.example.test","authGroup":"--script=x"}`, nil},
		"pin not pin-sha256":     {proto.TypeAnyConnect, `{"gateway":"vpn.example.test","serverCert":"sha1:00"}`, nil},
		"unknown field":          {proto.TypeAnyConnect, `{"gateway":"vpn.example.test","script":"/tmp/x"}`, nil},
		"password line break":    {proto.TypeAnyConnect, `{"gateway":"vpn.example.test"}`, map[string]string{"password": "a\nb"}},
	} {
		if _, err := prepare(c.typ, c.conf, c.secrets); err == nil {
			t.Errorf("%s: accepted", name)
		} else if c.secrets != nil && strings.Contains(err.Error(), c.secrets["password"]) {
			t.Errorf("%s: error names the password: %v", name, err)
		}
	}
}

func TestVersion(t *testing.T) {
	for out, want := range map[string]bool{
		"OpenConnect version v9.12\nUsing GnuTLS 3.8.4. Features present: ...\n": true,
		"OpenConnect version v9.21-unknown\n":                                    true,
		"OpenConnect version v10.00\n":                                           true,
		"OpenConnect version v9.01\n":                                            false,
		"OpenConnect version v8.20\n":                                            false,
		"Usage: openconnect [options] <server>\n":                                false,
	} {
		if err := checkVersion("/x/openconnect", out); (err == nil) != want {
			t.Errorf("%q: %v, want ok %v", out, err, want)
		}
	}
	err := checkVersion("/x/openconnect", "OpenConnect version v9.01\n")
	if err == nil || !strings.Contains(err.Error(), "9.12") || !strings.Contains(err.Error(), "9.01") {
		t.Fatalf("reason %v does not name both versions", err)
	}
}

func TestParseNet(t *testing.T) {
	n, err := parseNet(map[string]string{
		"INTERNAL_IP4_ADDRESS":           "10.9.0.2",
		"INTERNAL_IP4_MTU":               "1380",
		"INTERNAL_IP4_DNS":               "10.9.0.53 10.9.0.54",
		"CISCO_DEF_DOMAIN":               "corp.example, lab.example",
		"CISCO_SPLIT_INC":                "2",
		"CISCO_SPLIT_INC_0_ADDR":         "10.9.0.0",
		"CISCO_SPLIT_INC_0_MASKLEN":      "24",
		"CISCO_SPLIT_INC_1_ADDR":         "172.16.0.0",
		"CISCO_SPLIT_INC_1_MASKLEN":      "12",
		"INTERNAL_IP6_ADDRESS":           "fd00::2",
		"CISCO_IPV6_SPLIT_INC":           "1",
		"CISCO_IPV6_SPLIT_INC_0_ADDR":    "fd00::",
		"CISCO_IPV6_SPLIT_INC_0_MASKLEN": "64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(n.addrs, []netip.Addr{netip.MustParseAddr("10.9.0.2"), netip.MustParseAddr("fd00::2")}) ||
		!slices.Equal(n.address(), []string{"10.9.0.2/32", "fd00::2/128"}) ||
		!slices.Equal(n.dns, []netip.Addr{netip.MustParseAddr("10.9.0.53"), netip.MustParseAddr("10.9.0.54")}) ||
		!slices.Equal(n.search, []string{"corp.example", "lab.example"}) || n.mtu != 1380 ||
		!slices.Equal(n.routes, []string{"10.9.0.0/24", "172.16.0.0/12", "fd00::/64"}) {
		t.Fatalf("got %+v", n)
	}

	// Without split includes everything goes through the VPN.
	n, err = parseNet(map[string]string{"INTERNAL_IP4_ADDRESS": "10.9.0.2"})
	if err != nil || !slices.Equal(n.routes, []string{"0.0.0.0/0"}) || n.mtu != defaultMTU {
		t.Fatalf("got %+v, %v", n, err)
	}
	if _, err := parseNet(map[string]string{}); err == nil {
		t.Fatal("no address accepted")
	}
}

// On Windows the helper runs openconnect on its Wintun adapter (U12): the
// adapter by --interface, no --script-tun, and still no password in argv.
func TestSystemArgs(t *testing.T) {
	const script = `"C:\Program Files\HostbayNetd\hostbay-netd.exe" oc-script`
	args, err := SystemArgs(proto.TypeAnyConnect, `{"gateway":"vpn.example.test","user":"alice"}`, map[string]string{"password": testPass}, "Hostbay AB12CD34", script)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--protocol=anyconnect", "--non-inter", "--interface=Hostbay AB12CD34", "--script=" + script, "--user=alice", "--passwd-on-stdin", "--", "vpn.example.test"}
	if !slices.Equal(args, want) {
		t.Errorf("argv %q\nwant %q", args, want)
	}
	if _, err := SystemArgs(proto.TypeAnyConnect, `{"gateway":"vpn.example.test","browser":true}`, nil, "x", script); err == nil {
		t.Error("browser sign-in accepted system-wide")
	}
	if _, err := SystemArgs(proto.TypeAnyConnect, `{"gateway":"-vpn"}`, nil, "x", script); err == nil {
		t.Error("an option as the gateway accepted")
	}
}
