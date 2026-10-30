package netd

import "testing"

func TestIsTunnel(t *testing.T) {
	cases := []struct {
		goos, name, desc string
		p2p              bool
		ifType           uint32
		want             bool
	}{
		{goos: "darwin", name: "utun4", p2p: true, want: true},
		{goos: "darwin", name: "ppp0", p2p: true, want: true},
		{goos: "darwin", name: "ipsec0", p2p: true, want: true},
		{goos: "darwin", name: "en0", want: false},
		{goos: "darwin", name: "gif0", p2p: true, want: false}, // dormant 6in4
		{goos: "darwin", name: "utun4", p2p: false, want: false},
		{goos: "linux", name: "wg0", p2p: true, want: true},
		{goos: "linux", name: "cscotun0", p2p: true, want: true},
		{goos: "linux", name: "tun0", p2p: false, want: false},
		{goos: "linux", name: "eth0", want: false},
		{goos: "windows", name: "Ethernet", ifType: 6, desc: "Intel(R) Ethernet Connection", want: false},
		{goos: "windows", name: "Wi-Fi", ifType: 71, desc: "Intel(R) Wi-Fi 6", want: false},
		{goos: "windows", name: "OpenVPN Wintun", ifType: 53, desc: "Wintun Userspace Tunnel", want: true},
		{goos: "windows", name: "Ethernet 3", ifType: 6, desc: "Cisco AnyConnect Secure Mobility Client Virtual Miniport Adapter for Windows x64", want: true},
		{goos: "windows", name: "Ethernet 4", ifType: 6, desc: "TAP-Windows Adapter V9", want: true},
		{goos: "windows", name: "vEthernet (WSL)", ifType: 6, desc: "Hyper-V Virtual Ethernet Adapter", want: false},
		{goos: "windows", name: "PPP adapter", ifType: 23, want: true},
	}
	for _, c := range cases {
		if got := isTunnel(c.goos, c.name, c.p2p, c.ifType, c.desc); got != c.want {
			t.Errorf("isTunnel(%s, %q) = %v, want %v", c.goos, c.name, got, c.want)
		}
	}
}
