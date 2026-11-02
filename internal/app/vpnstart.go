package app

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
	"hostbay/internal/vpn/proto"
)

const wireGuardPort = 51820

// nodeDir is a Tailscale profile's own folder, which holds what tsnet
// writes besides the node's keys. A profile ID from sync is not trusted as
// a path.
func (s *VPNService) nodeDir(id string) string {
	name := id
	if !safeName.MatchString(id) {
		sum := sha256.Sum256([]byte(id))
		name = hex.EncodeToString(sum[:])
	}
	return filepath.Join(s.st.Dir(), "vpn", name)
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// checkSaved refuses a profile that sync or a restore saved with a value no
// config may carry, such as a line break that would add a WireGuard peer.
func checkSaved(v *store.VPN) error {
	if err := store.CheckVPN(*v); err != nil {
		return apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s cannot connect: %v; fix it in the profile's settings", v.Name, err)).
			With("vpnId", v.ID).With("reason", "invalid")
	}
	return nil
}

// openConnectConfig and tailnetConfig are the engines' Config, field for
// field: each refuses or ignores anything else.
type openConnectConfig struct {
	Gateway    string `json:"gateway"`
	User       string `json:"user,omitempty"`
	ServerCert string `json:"serverCert,omitempty"`
	Browser    bool   `json:"browser,omitempty"`
	AuthGroup  string `json:"authGroup,omitempty"`
}

type tailnetConfig struct {
	Dir        string `json:"dir"`
	ControlURL string `json:"controlURL"`
	Hostname   string `json:"hostname"`
}

// startMsg builds the helper's start message from the saved profile, with
// its secrets opened and a fresh SOCKS login. It returns the profile as it
// was read.
func (s *VPNService) startMsg(id string) (proto.Msg, *store.VPN, error) {
	d, err := s.st.Data()
	if err != nil {
		return proto.Msg{}, nil, err
	}
	v := d.VPN(id)
	m, err := s.startMsgFor(id, v)
	if err != nil {
		return proto.Msg{}, nil, err
	}
	return m, v, nil
}

// startMsgFor is startMsg from profile id as already read, nil when it is
// gone.
func (s *VPNService) startMsgFor(id string, v *store.VPN) (proto.Msg, error) {
	if v == nil {
		return proto.Msg{}, apperr.New(apperr.VPNFailed, "This VPN profile no longer exists").With("vpnId", id).With("reason", "missing")
	}
	if err := checkSaved(v); err != nil {
		return proto.Msg{}, err
	}
	if v.Tier != store.VPNTierApp || v.Type != proto.TypeWireGuard && v.Type != proto.TypeTailscale && !proto.IsOpenConnect(v.Type) {
		return proto.Msg{}, apperr.New(apperr.VPNFailed, fmt.Sprintf("Hostbay cannot connect %s profiles (tier %s) yet", v.Type, v.Tier)).
			With("vpnId", id).With("reason", "unsupported")
	}
	if proto.IsOpenConnect(v.Type) && v.Auth == "certificate" {
		return proto.Msg{}, apperr.New(apperr.VPNFailed, "Hostbay cannot sign in to this VPN with a client certificate yet; choose another sign-in in the profile").
			With("vpnId", id).With("reason", "unsupported")
	}
	sec, err := s.openSecrets(v)
	if err != nil {
		return proto.Msg{}, err
	}
	m := proto.Msg{T: proto.Start, Type: v.Type, SocksUser: rand.Text(), SocksPass: rand.Text()}
	switch v.Type {
	case proto.TypeWireGuard:
		m.Config = wireGuardConfig(v)
		m.Secrets = map[string]string{"privateKey": sec.PrivateKey, "presharedKey": sec.PresharedKey}
	case proto.TypeTailscale:
		dir := s.nodeDir(id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return proto.Msg{}, err
		}
		node, err := s.nodeState(v)
		if err != nil {
			return proto.Msg{}, err
		}
		host, _ := os.Hostname()
		b, _ := json.Marshal(tailnetConfig{Dir: dir, ControlURL: v.Gateway, Hostname: nodeHostname(host)})
		m.Config = string(b)
		m.Secrets = map[string]string{"authKey": sec.AuthKey, "nodeState": node}
	default:
		c := openConnectConfig{Gateway: openConnectGateway(v.Gateway, v.Port), User: v.User, ServerCert: v.PeerKey, AuthGroup: v.AuthGroup,
			Browser: v.Type == proto.TypeAnyConnect && v.Auth == "browser"}
		pass := sec.Password
		if c.Browser {
			pass = "" // a stray line would answer openconnect's next prompt
		}
		b, _ := json.Marshal(c)
		m.Config = string(b)
		m.Secrets = map[string]string{"password": pass}
	}
	return m, nil
}

// nodeState opens the Tailscale profile's saved node. One that no longer
// opens (sealed for another control server, say) is forgotten: the node
// signs in again.
func (s *VPNService) nodeState(v *store.VPN) (string, error) {
	l, err := s.st.Local()
	if err != nil || l.VPNNodeStates[v.ID] == "" {
		return "", err
	}
	b, err := s.vault.Open(l.VPNNodeStates[v.ID], v.NodeStateAAD())
	if undecryptable(err) {
		return "", s.st.EditLocal(func(l *store.Local) error {
			delete(l.VPNNodeStates, v.ID)
			return nil
		})
	}
	return string(b), err
}

// nodeHostname is a Tailscale node's name from this computer's host name:
// one DNS label ending in -hostbay.
func nodeHostname(host string) string {
	const suffix = "-hostbay"
	host, _, _ = strings.Cut(host, ".")
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, strings.ToLower(host))
	name = strings.Trim(name, "-")
	if len(name) > 63-len(suffix) {
		name = strings.TrimRight(name[:63-len(suffix)], "-")
	}
	if name == "" {
		return "hostbay"
	}
	return name + suffix
}

// openConnectGateway adds port to gw unless gw names one: openconnect takes
// host, host:port and either followed by /path.
func openConnectGateway(gw string, port int) string {
	host, path, slash := strings.Cut(gw, "/")
	if _, _, err := net.SplitHostPort(host); err != nil && port != 0 {
		host = net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
	}
	if slash {
		return host + "/" + path
	}
	return host
}

// wireGuardConfig writes the profile as a wg-quick .conf without its keys,
// which travel in the start message's secrets.
func wireGuardConfig(v *store.VPN) string {
	var b strings.Builder
	line := func(k string, vs ...string) {
		if len(vs) > 0 && vs[0] != "" {
			fmt.Fprintf(&b, "%s = %s\n", k, strings.Join(vs, ", "))
		}
	}
	b.WriteString("[Interface]\n")
	line("Address", v.Address...)
	line("DNS", append(slices.Clone(v.DNSServers), v.DNSDomains...)...)
	line("MTU", strconv.Itoa(v.TunnelMTU()))
	b.WriteString("[Peer]\n")
	line("PublicKey", v.PeerKey)
	port := v.Port
	if port == 0 {
		port = wireGuardPort
	}
	line("Endpoint", net.JoinHostPort(v.Gateway, strconv.Itoa(port)))
	line("AllowedIPs", v.Routes...)
	return b.String()
}
