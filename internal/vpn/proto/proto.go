// Package proto is the protocol between Hostbay and its hostbay-vpn helper
// (KTD1): newline-delimited JSON over the helper's stdin and stdout, each
// message tagged by "t".
//
// The helper opens with hello and its version. Hostbay sends start, which
// carries the profile's secrets and the per-run SOCKS login: stdin is the
// only place they go. The helper then sends state as the tunnel comes up,
// changes or ends, and need when the engine waits on the user, which Hostbay
// meets with answer. A Tailscale helper also sends nodeState whenever its
// node's keys change, for Hostbay to seal and send back in the next start's
// secrets. Hostbay may send openBrowser at any time; the helper answers
// browserPort with the port of a second SOCKS5 listener on 127.0.0.1 that
// takes no login, for a browser profile (Chromium cannot log in to a SOCKS
// proxy, KTD11). Later requests get the same port. Closing the helper's
// stdin disconnects the profile and the helper exits, closing both
// listeners.
//
// This package is imported by the main binary, so it uses only the standard
// library.
package proto

// Version is bumped on any incompatible change to the messages below.
const Version = 1

// Message tags.
const (
	Hello  = "hello"  // helper: Version
	Start  = "start"  // Hostbay: Type, Config, Secrets, SocksUser, SocksPass
	State  = "state"  // helper: State, Reason, Address, Routes, SocksPort, Code, ServerCert
	Need   = "need"   // helper: Need, URL
	Answer = "answer" // Hostbay: OTP
	// NodeState carries secrets, unlike every other helper message: Hostbay
	// seals it, keeps it on this device only and never logs it.
	NodeState = "nodeState" // helper: NodeState
	// The browser listener's port, like SocksPort, stays between Hostbay and
	// the helper: no state, event, log or MCP reply carries it.
	OpenBrowser = "openBrowser" // Hostbay: nothing
	BrowserPort = "browserPort" // helper: SocksPort, or Reason when it could not open
)

// Engine types. The OpenConnect ones run the openconnect program; their
// names are neutral, since the UI never names the SSL VPN's vendor.
const (
	TypeWireGuard      = "wireguard"
	TypeAnyConnect     = "anyconnect"
	TypeSSLVPN         = "sslvpn" // "SSL VPN (PPP over TLS)"
	TypeGlobalProtect  = "globalprotect"
	TypePulse          = "pulse"
	TypeNetworkConnect = "nc" // Pulse's older Network Connect protocol
	TypeF5             = "f5"
	TypeArray          = "array"
	TypeTailscale      = "tailscale" // Tailscale, or Headscale through its control URL
)

// OpenConnectTypes is every engine type that runs the openconnect program,
// each with its --protocol value.
var OpenConnectTypes = map[string]string{
	TypeAnyConnect:     "anyconnect",
	TypeSSLVPN:         "fortinet",
	TypeGlobalProtect:  "gp",
	TypePulse:          "pulse",
	TypeNetworkConnect: "nc",
	TypeF5:             "f5",
	TypeArray:          "array",
}

// IsOpenConnect reports whether t runs the openconnect program.
func IsOpenConnect(t string) bool {
	_, ok := OpenConnectTypes[t]
	return ok
}

// Types that connect only system-wide, through hostbay-netd or the
// operating system, never inside hostbay-vpn.
const (
	TypeOpenVPN = "openvpn"
	TypeIPsec   = "ipsec" // IKEv2 and L2TP
)

// Tunnel states.
const (
	Connecting = "connecting"
	Connected  = "connected" // Address, Routes and SocksPort are set
	Dropped    = "dropped"   // was connected; Reason says why it ended
	Failed     = "failed"    // never connected; Reason says why
)

// What a need message waits for.
const (
	NeedOTP     = "otp"     // a one-time code, sent back as answer
	NeedBrowser = "browser" // the user signs in at URL
)

// Codes on a failed state, for a reason the app acts on rather than only
// shows.
const (
	CodeNeedsSystemWide = "needs-system-wide"   // this platform cannot run the type app-scoped
	CodeCertUnknown     = "cert-unknown"        // the system does not trust the gateway; ServerCert is its pin
	CodeCertChanged     = "cert-changed"        // the gateway is not the pinned one; ServerCert is what it presented
	CodeOpenConnect     = "openconnect-version" // the openconnect found is too old, or its version cannot be read
)

// Msg is any message in either direction. State messages never carry a
// secret: Reason, Address and Routes are safe to show and log.
type Msg struct {
	T string `json:"t"`

	Version int `json:"version,omitempty"`

	// Type is the engine; Config is the profile's non-secret configuration in
	// that engine's own format (a wg-quick .conf for WireGuard); Secrets are
	// the sealed profile secrets by name (WireGuard: privateKey,
	// presharedKey; Tailscale: authKey, nodeState).
	Type      string            `json:"type,omitempty"`
	Config    string            `json:"config,omitempty"`
	Secrets   map[string]string `json:"secrets,omitempty"`
	SocksUser string            `json:"socksUser,omitempty"`
	SocksPass string            `json:"socksPass,omitempty"`

	State     string   `json:"state,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Address   []string `json:"address,omitempty"` // the tunnel's own addresses, CIDR
	Routes    []string `json:"routes,omitempty"`  // networks reached through it, CIDR
	SocksPort int      `json:"socksPort,omitempty"`
	Code      string   `json:"code,omitempty"`
	// ServerCert is the gateway certificate's pin ("pin-sha256:..."), set
	// with CodeCertUnknown and CodeCertChanged. Hostbay pins it only after
	// the user accepts it (R21).
	ServerCert string `json:"serverCert,omitempty"`

	Need string `json:"need,omitempty"`
	URL  string `json:"url,omitempty"`
	OTP  string `json:"otp,omitempty"`

	// NodeState is a Tailscale node's whole state store, keys included,
	// base64. Hostbay passes it back unchanged as secrets["nodeState"].
	NodeState string `json:"nodeState,omitempty"`
}
