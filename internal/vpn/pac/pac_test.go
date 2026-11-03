package pac

import (
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var (
	odInternal = Profile{ID: "od", Name: "OD Internal", Port: 1080, Domains: []string{"corp.example"}, Networks: []string{"10.212.0.0/16", "172.31.58.0/24"}}
	branch     = Profile{ID: "br", Name: "Branch", Port: 2080, Domains: []string{"wiki.corp.example", "branch.example"}, Networks: []string{"10.0.0.0/8"}}
)

// find answers host as the script does, by running it in node when node is
// installed, with the two PAC helpers the script calls.
func find(t *testing.T, script string, hosts ...string) map[string]string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	runner := script + `
function dnsDomainIs(h, d) { return h.length >= d.length && h.substring(h.length - d.length) === d; }
function ip4(s) { return s.split(".").reduce(function (a, o) { return a * 256 + Number(o); }, 0); }
function isInNet(h, n, m) { var x = ip4(h), y = ip4(n), z = ip4(m); for (var i = 0; i < 32; i++) { var b = Math.pow(2, 31 - i); if (Math.floor(z / b) % 2 && Math.floor(x / b) % 2 !== Math.floor(y / b) % 2) return false; } return true; }
function dnsResolve() { throw new Error("the PAC resolved a name"); }
function myIpAddress() { throw new Error("the PAC read the local address"); }
var out = {};
JSON.parse(process.argv[2]).forEach(function (h) { out[h] = FindProxyForURL("https://" + h + "/", h); });
console.log(JSON.stringify(out));
`
	f := filepath.Join(t.TempDir(), "pac.js")
	if err := os.WriteFile(f, []byte(runner), 0o600); err != nil {
		t.Fatal(err)
	}
	arg, _ := json.Marshal(hosts)
	b, err := exec.Command(node, f, string(arg)).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	got := map[string]string{}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("node said %s", b)
	}
	return got
}

func expect(t *testing.T, got, want map[string]string) {
	t.Helper()
	for h, w := range want {
		if got[h] != w {
			t.Errorf("%s: %q, want %q", h, got[h], w)
		}
	}
}

// A listed domain and its subdomains, and an IPv4 address in a profile's
// networks, go to that profile's listener; everything else goes direct.
func TestBuildSendsListedSitesAndNetworksToTheirVPN(t *testing.T) {
	r := Build([]Profile{odInternal})
	od := "SOCKS5 127.0.0.1:1080"
	expect(t, find(t, r.Script, "corp.example", "WIKI.Corp.Example", "wiki.corp.example.", "notcorp.example", "10.212.4.5", "172.31.58.9", "ipinfo.io", "8.8.8.8", "10.213.0.1"), map[string]string{
		"corp.example": od, "WIKI.Corp.Example": od, "wiki.corp.example.": od, "notcorp.example": "DIRECT",
		"10.212.4.5": od, "172.31.58.9": od, "ipinfo.io": "DIRECT", "8.8.8.8": "DIRECT", "10.213.0.1": "DIRECT",
	})
	if r.Domains["od"] != 1 || r.Networks["od"] != 2 || len(r.Skipped) != 0 {
		t.Fatalf("counts %v %v, skipped %v", r.Domains, r.Networks, r.Skipped)
	}
}

// The most specific domain or network wins, whichever profile lists it.
func TestBuildPrefersTheMostSpecificEntry(t *testing.T) {
	r := Build([]Profile{branch, odInternal})
	expect(t, find(t, r.Script, "wiki.corp.example", "x.wiki.corp.example", "mail.corp.example", "10.212.1.1", "10.1.1.1"), map[string]string{
		"wiki.corp.example": "SOCKS5 127.0.0.1:2080", "x.wiki.corp.example": "SOCKS5 127.0.0.1:2080", "mail.corp.example": "SOCKS5 127.0.0.1:1080",
		"10.212.1.1": "SOCKS5 127.0.0.1:1080", "10.1.1.1": "SOCKS5 127.0.0.1:2080",
	})
}

// A domain or network two profiles both list goes to the first, and the
// other is reported skipped.
func TestBuildGivesADuplicateToTheFirstProfile(t *testing.T) {
	other := Profile{ID: "x", Name: "Other", Port: 3080, Domains: []string{"corp.example"}, Networks: []string{"10.212.0.0/16"}}
	r := Build([]Profile{odInternal, other})
	expect(t, find(t, r.Script, "corp.example", "10.212.0.9"), map[string]string{"corp.example": "SOCKS5 127.0.0.1:1080", "10.212.0.9": "SOCKS5 127.0.0.1:1080"})
	if len(r.Skipped) != 2 || r.Skipped[0].Profile != "x" || r.Skipped[1].Profile != "x" || r.Domains["x"] != 0 || r.Networks["x"] != 0 {
		t.Fatalf("skipped %+v, counts %v %v", r.Skipped, r.Domains, r.Networks)
	}
}

// A full tunnel's default route is not a network here: every name already
// goes direct, and LAN addresses must too.
func TestBuildLeavesTheLocalNetworkDirectBesideAFullTunnel(t *testing.T) {
	full := Profile{ID: "f", Name: "Full", Port: 4080, Domains: []string{"corp.example"}, Networks: []string{"0.0.0.0/0", "0.0.0.0/1", "128.0.0.0/1", "::/0", "fd00::/8"}}
	r := Build([]Profile{full})
	expect(t, find(t, r.Script, "192.168.1.1", "127.0.0.2", "corp.example"), map[string]string{"192.168.1.1": "DIRECT", "127.0.0.2": "DIRECT", "corp.example": "SOCKS5 127.0.0.1:4080"})
	if r.Networks["f"] != 0 {
		t.Fatalf("networks %v", r.Networks)
	}
}

// Nothing to route is no script, so the caller removes the system setting.
func TestBuildWithNothingToRouteIsEmpty(t *testing.T) {
	if r := Build(nil); r.Script != "" {
		t.Fatalf("script %q", r.Script)
	}
	if r := Build([]Profile{{ID: "e", Name: "Empty", Port: 1, Networks: []string{"0.0.0.0/0"}}}); r.Script != "" {
		t.Fatalf("script %q", r.Script)
	}
}

// The script never resolves a name, never falls back to direct or SOCKS v4
// after a match, and carries only checked names and networks.
func TestBuildWritesOnlySafeEntries(t *testing.T) {
	r := Build([]Profile{{ID: "b", Name: "Bad", Port: 1080, Domains: []string{`x"); evil("`, "*.corp.example", "ok.example"}, Networks: []string{"10.0.0.0/8\"", "nope", "10.1.2.3/16"}}})
	for _, bad := range []string{"dnsResolve", "myIpAddress", "evil", "*", "SOCKS ", "; DIRECT"} {
		if strings.Contains(r.Script, bad) {
			t.Errorf("script contains %q:\n%s", bad, r.Script)
		}
	}
	if r.Domains["b"] != 1 || r.Networks["b"] != 1 || len(r.Skipped) != 4 {
		t.Fatalf("counts %v %v, skipped %+v", r.Domains, r.Networks, r.Skipped)
	}
	expect(t, find(t, r.Script, "10.1.9.9"), map[string]string{"10.1.9.9": "SOCKS5 127.0.0.1:1080"})
}

// A pushed network wider than the LAN leaves the LAN's own addresses
// direct, and a network listed inside the LAN still goes to its VPN.
func TestBuildLeavesOverlappedLocalNetworksDirect(t *testing.T) {
	wide := Profile{ID: "w", Name: "Wide", Port: 3080, Networks: []string{"192.168.0.0/16", "192.168.1.128/25"}}
	r := Build([]Profile{wide}, netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("172.16.5.0/24"))
	if strings.Contains(r.Script, "172.16.5.0") || r.Networks["w"] != 2 {
		t.Fatalf("networks %v\n%s", r.Networks, r.Script)
	}
	expect(t, find(t, r.Script, "192.168.1.7", "192.168.1.200", "192.168.9.9"), map[string]string{
		"192.168.1.7":   "DIRECT",
		"192.168.1.200": "SOCKS5 127.0.0.1:3080",
		"192.168.9.9":   "SOCKS5 127.0.0.1:3080",
	})
}
