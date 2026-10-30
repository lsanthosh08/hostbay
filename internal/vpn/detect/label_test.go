package detect

import "testing"

func p2p(iface, local, peer string) Tunnel {
	return Tunnel{Iface: iface, Local: local, Peer: peer, Shape: PointToPoint}
}

func peerless(iface, local string) Tunnel {
	return Tunnel{Iface: iface, Local: local, Shape: PeerLess}
}

// labels is Label's result as interface -> "profile by".
func labels(tuns []Tunnel, ps []Profile) map[string]string {
	out := map[string]string{}
	for _, l := range Label(tuns, ps) {
		if l.Profile != "" {
			out[l.Iface] = l.Profile + " " + l.By
		}
	}
	return out
}

func TestTwoPointToPointVPNsStayUnlabeled(t *testing.T) {
	// AE4: two point-to-point tunnels, two peer-shaped profiles with no range
	// or pin: shape cannot tell them apart, so neither is guessed.
	tuns := []Tunnel{p2p("utun4", "10.255.0.54", "10.255.0.54"), p2p("utun6", "172.16.9.2", "172.16.9.1")}
	ps := []Profile{{ID: "a", Shape: PointToPoint}, {ID: "b", Shape: PointToPoint}}
	if got := labels(tuns, ps); len(got) != 0 {
		t.Fatalf("labeled %v, want none", got)
	}
	for _, l := range Label(tuns, ps) {
		if l.Profile != "" || l.By != "" {
			t.Fatalf("%s labeled %q by %q", l.Iface, l.Profile, l.By)
		}
	}
}

func TestOnePeerLessTunnelAndProfileLabelTogether(t *testing.T) {
	tuns := []Tunnel{peerless("utun7", "10.212.134.200"), p2p("utun4", "10.255.0.54", "10.255.0.54"), p2p("utun6", "172.16.9.2", "172.16.9.1")}
	ps := []Profile{{ID: "ssl", Shape: PeerLess}, {ID: "a", Shape: PointToPoint}, {ID: "b", Shape: PointToPoint}}
	got := labels(tuns, ps)
	if want := map[string]string{"utun7": "ssl shape"}; !mapsEqual(got, want) {
		t.Fatalf("labels %v, want %v", got, want)
	}
}

func TestPinnedInterfaceClaimsItUnderAmbiguity(t *testing.T) {
	tuns := []Tunnel{p2p("utun4", "10.255.0.54", "10.255.0.54"), p2p("utun6", "172.16.9.2", "172.16.9.1")}
	ps := []Profile{{ID: "a", Shape: PointToPoint, Iface: "utun6"}, {ID: "b", Shape: PointToPoint}}
	// a's pin takes utun6; b is then the one peer profile left for the one
	// point-to-point tunnel left.
	want := map[string]string{"utun6": "a pin", "utun4": "b shape"}
	if got := labels(tuns, ps); !mapsEqual(got, want) {
		t.Fatalf("labels %v, want %v", got, want)
	}
}

func TestAddressRangeLabelsOnlyWithOneClaimant(t *testing.T) {
	tuns := []Tunnel{p2p("utun4", "10.255.0.54", "10.255.0.54"), p2p("utun6", "172.16.9.2", "172.16.9.1")}
	ps := []Profile{
		{ID: "sase", Shape: PointToPoint, Ranges: []string{"10.255.0.0/16"}},
		{ID: "full", Shape: PointToPoint, Ranges: []string{"0.0.0.0/0", "0.0.0.0/1", "128.0.0.0/1"}}, // never claims by range
	}
	want := map[string]string{"utun4": "sase range", "utun6": "full shape"}
	if got := labels(tuns, ps); !mapsEqual(got, want) {
		t.Fatalf("labels %v, want %v", got, want)
	}
	// Two profiles whose ranges both hold the tunnel's address: neither.
	ps = []Profile{{ID: "x", Ranges: []string{"10.0.0.0/8"}}, {ID: "y", Ranges: []string{"10.255.0.0/16"}}}
	if got := labels(tuns[:1], ps); len(got) != 0 {
		t.Fatalf("labeled %v, want none", got)
	}
}

func TestOSNameComesFirstAndNeverGuesses(t *testing.T) {
	forti := Tunnel{Iface: "utun7", Local: "10.212.134.200", Shape: PeerLess, VPN: "FortiClient VPN"}
	tuns := []Tunnel{forti, p2p("utun4", "10.255.0.54", "10.255.0.54")}
	ps := []Profile{
		{ID: "f", Names: []string{"Office", "forticlient vpn"}, Shape: PeerLess, Iface: "utun4"},
		{ID: "s", Shape: PointToPoint},
	}
	// The OS's own name outranks f's (stale) pin; s then takes utun4 by shape.
	want := map[string]string{"utun7": "f os", "utun4": "s shape"}
	if got := labels(tuns, ps); !mapsEqual(got, want) {
		t.Fatalf("labels %v, want %v", got, want)
	}
	// Two profiles answering to the OS's name: neither takes it by name.
	ps = []Profile{{ID: "f1", Names: []string{"FortiClient VPN"}}, {ID: "f2", Names: []string{"FortiClient VPN"}}}
	if got := labels([]Tunnel{forti}, ps); len(got) != 0 {
		t.Fatalf("labeled %v, want none", got)
	}
}

func TestUnknownShapeIsNeverLabeledByShape(t *testing.T) {
	// Linux and Windows report no shape: one tunnel and one profile still
	// stay apart without a name, range or pin.
	tuns := []Tunnel{{Iface: "tun0", Local: "10.8.0.6"}}
	if got := labels(tuns, []Profile{{ID: "a"}}); len(got) != 0 {
		t.Fatalf("labeled %v, want none", got)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestShapeAndTypeTables(t *testing.T) {
	for _, c := range []struct{ vendor, typ, shape string }{
		{"com.fortinet.forticlient.vpn", "sslvpn", PeerLess},
		{"Harmony SASE", "perimeter81", PointToPoint},
		{"com.cisco.anyconnect", "anyconnect", PointToPoint},
		{"com.wireguard.macos", "wireguard", PointToPoint},
		{"OpenVPNConnect", "openvpn", PointToPoint},
		{"Some VPN", "other", ""},
	} {
		if got := TypeFor(c.vendor); got != c.typ {
			t.Errorf("TypeFor(%q) = %q, want %q", c.vendor, got, c.typ)
		}
		if got := ShapeFor(c.typ); got != c.shape {
			t.Errorf("ShapeFor(%q) = %q, want %q", c.typ, got, c.shape)
		}
	}
}
