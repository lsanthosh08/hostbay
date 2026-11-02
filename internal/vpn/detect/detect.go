// Package detect finds the vendor VPNs connected on this computer and the
// tunnel interfaces they bring up (R22), and labels each tunnel with the
// external profile it belongs to without ever guessing (R24, KTD7). It runs
// only the operating system's own read-only tools; it changes nothing.
package detect

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Tunnel shapes on macOS, where TunnelHub found them to tell VPNs apart
// without knowing any account's addresses.
const (
	PointToPoint = "peer"     // utun with a peer: inet X --> Y
	PeerLess     = "peerless" // utun with only its own address, as the SSL VPN client brings up
)

// Tunnel is one tunnel interface up on this computer with an IPv4 address.
type Tunnel struct {
	Iface string `json:"iface"`           // the OS's interface name: utun4, tun0, "Ethernet 3"
	Local string `json:"local"`           // its own IPv4 address
	Peer  string `json:"peer,omitempty"`  // point-to-point: the far end
	Shape string `json:"shape,omitempty"` // PointToPoint or PeerLess; "" where the OS does not tell
	VPN   string `json:"vpn,omitempty"`   // the name the OS itself gives the VPN that owns it
}

// VPN is a VPN client found on this computer, connected or not.
type VPN struct {
	Name      string `json:"name"`            // the name the OS or the vendor's app shows
	Type      string `json:"type"`            // the profile type it maps to (TypeFor)
	Shape     string `json:"shape,omitempty"` // ShapeFor(Type)
	Connected bool   `json:"connected"`
	Iface     string `json:"iface,omitempty"`  // connected: the interface the OS names for it
	Client    string `json:"client,omitempty"` // the app that runs it, when detection saw its path
}

// Snapshot is one detection pass.
type Snapshot struct {
	VPNs    []VPN    `json:"vpns"`
	Tunnels []Tunnel `json:"tunnels"`
}

// Detect looks at this computer now.
func Detect(ctx context.Context) (Snapshot, error) { return detect(ctx) }

// vendors maps a word in a vendor's id or app name to the profile type it
// is, first match wins. Only the SSL VPN client's tunnel has no peer.
var vendors = []struct{ word, typ string }{
	{"forti", "sslvpn"},
	{"cisco", "anyconnect"}, {"anyconnect", "anyconnect"}, {"secureclient", "anyconnect"}, {"secure client", "anyconnect"},
	{"paloalto", "globalprotect"}, {"globalprotect", "globalprotect"}, {"pangp", "globalprotect"},
	{"pulse", "pulse"}, {"ivanti", "pulse"}, {"juniper", "pulse"},
	{"perimeter81", "perimeter81"}, {"perimeter 81", "perimeter81"}, {"harmony", "perimeter81"}, {"check point sase", "perimeter81"}, {"safervpn", "perimeter81"},
	{"wireguard", "wireguard"}, {"tailscale", "tailscale"}, {"zerotier", "zerotier"}, {"openvpn", "openvpn"},
}

// TypeFor is the profile type of a VPN known by s: a reverse-DNS id such as
// com.fortinet.forticlient.vpn, an app name or an adapter description.
func TypeFor(s string) string {
	s = strings.ToLower(s)
	for _, v := range vendors {
		if strings.Contains(s, v.word) {
			return v.typ
		}
	}
	return "other"
}

// ShapeFor is the shape of the tunnel a profile type brings up on macOS,
// or "" when it is not known, so shape alone never labels it.
func ShapeFor(typ string) string {
	switch typ {
	case "sslvpn":
		return PeerLess
	case "anyconnect", "globalprotect", "pulse", "perimeter81", "wireguard", "tailscale", "openvpn":
		return PointToPoint
	}
	return ""
}

// run is a read-only tool's output; a tool that is missing or fails is no
// output, so one missing source never hides the others.
func run(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, name, args...).Output()
	return string(out)
}

// vendorCLIs are the vendor command lines that report state, where they
// are installed (R3): Cisco's prints "state: Connected".
var vendorCLIs = map[string][]string{
	"anyconnect": {
		"/opt/cisco/secureclient/bin/vpn", "/opt/cisco/anyconnect/bin/vpn",
		`C:\Program Files (x86)\Cisco\Cisco Secure Client\vpncli.exe`, `C:\Program Files (x86)\Cisco\Cisco AnyConnect Secure Mobility Client\vpncli.exe`,
	},
}

// CLIConnected asks the vendor's CLI for typ whether its VPN is connected,
// where one is installed. Detection itself never runs it, so a slow CLI
// holds up no dial.
func CLIConnected(ctx context.Context, typ string) bool {
	for _, p := range vendorCLIs[typ] {
		if _, err := os.Stat(p); err == nil {
			return strings.Contains(strings.ToLower(run(ctx, p, "state")), "state: connected")
		}
	}
	return false
}

func containsAny(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
