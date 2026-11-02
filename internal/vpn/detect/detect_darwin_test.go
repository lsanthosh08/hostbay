package detect

import (
	"reflect"
	"testing"
)

// What this computer's tools print, trimmed; addresses are made up.
const (
	ncList = `Available network connection services in the current set (*=enabled):
* (Disconnected)   365203FE-0000-0000-0000-000000000000 VPN (com.fortinet.forticlient.vpn) "FortiClient VPN"                [VPN:com.fortinet.forticlient.vpn]
* (Connected)      11111111-0000-0000-0000-000000000000 VPN (com.wireguard.macos) "Lab WG"                [VPN:com.wireguard.macos]
`
	ncStatus = `Connected
Extended Status <dictionary> {
  IPv4 : <dictionary> {
    InterfaceName : utun9
  }
}
`
	ifconfigOut = `lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384
	inet 127.0.0.1 netmask 0xff000000
en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet 192.168.1.20 netmask 0xffffff00 broadcast 192.168.1.255
utun0: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1500
	inet6 fe80::1%utun0 prefixlen 64 scopeid 0x10
utun4: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1280
	inet 10.200.0.54 --> 10.200.0.54 netmask 0xff000000
utun7: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1400
	inet 10.212.1.5 netmask 0xffffffff
utun9: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1420
	inet 10.9.0.2 --> 10.9.0.2 netmask 0xffffffff
`
	psOut = `COMMAND
/sbin/launchd
/Library/Frameworks/OpenVPNConnect.framework/Versions/Current/usr/sbin/ovpnagent
/Applications/Harmony SASE.app/Contents/MacOS/Harmony SASE -f
/System/Library/ExtensionKit/Extensions/vpn.appex/Contents/MacOS/vpn
/usr/libexec/neagent
`
)

func TestDarwinDetectsSystemDaemonAndAgentVPNs(t *testing.T) {
	status := map[string]string{"Lab WG": ncStatus}
	s := fromDarwin(ncList, status, ifconfigOut, psOut)
	wantTuns := []Tunnel{
		{Iface: "utun4", Local: "10.200.0.54", Peer: "10.200.0.54", Shape: PointToPoint},
		{Iface: "utun7", Local: "10.212.1.5", Shape: PeerLess},
		{Iface: "utun9", Local: "10.9.0.2", Peer: "10.9.0.2", Shape: PointToPoint, VPN: "Lab WG"},
	}
	if !reflect.DeepEqual(s.Tunnels, wantTuns) {
		t.Fatalf("tunnels\n got %+v\nwant %+v", s.Tunnels, wantTuns)
	}
	wantVPNs := []VPN{
		{Name: "FortiClient VPN", Type: "sslvpn", Shape: PeerLess},
		{Name: "Lab WG", Type: "wireguard", Shape: PointToPoint, Connected: true, Iface: "utun9"},
		// The one agent with the one point-to-point tunnel nothing else
		// explains; the OpenVPN daemon running says nothing about a tunnel.
		{Name: "Harmony SASE", Type: "perimeter81", Shape: PointToPoint, Connected: true, Client: "/Applications/Harmony SASE.app"},
		{Name: "OpenVPNConnect", Type: "openvpn", Shape: PointToPoint},
	}
	if !reflect.DeepEqual(s.VPNs, wantVPNs) {
		t.Fatalf("vpns\n got %+v\nwant %+v", s.VPNs, wantVPNs)
	}
}

// An app whose arguments mention vpn is not a VPN agent: Claude's helper was
// taken for one, which also made a real agent look disconnected.
func TestDarwinAppsAreAgentsByTheirNameNotTheirArguments(t *testing.T) {
	ps := psOut + "/Applications/Claude.app/Contents/Helpers/disclaimer /x/claude-code --vpn-check\n"
	for _, v := range fromDarwin(ncList, map[string]string{"Lab WG": ncStatus}, ifconfigOut, ps).VPNs {
		if v.Name == "Claude" {
			t.Fatalf("Claude taken for a VPN: %+v", v)
		}
	}
}

func TestDarwinAgentsOutnumberingTunnelsAreNotConnected(t *testing.T) {
	ps := psOut + "/Applications/Other VPN.app/Contents/MacOS/Other VPN\n"
	s := fromDarwin(ncList, map[string]string{"Lab WG": ncStatus}, ifconfigOut, ps)
	for _, v := range s.VPNs {
		if v.Connected && v.Name != "Lab WG" {
			t.Fatalf("%s reported connected with two agents and one unexplained point-to-point tunnel", v.Name)
		}
	}
}
