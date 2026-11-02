package openvpn

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/importer/vpnconf"
	"hostbay/internal/store"
)

const pem = "-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIUQ\n-----END CERTIFICATE-----"

const (
	caBlock     = "<ca>\n" + pem + "\n</ca>"
	fingerprint = "AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99"
)

var unixMgmt = Management{Socket: "/Library/PrivilegedHelperTools/dev.hostbay.netd/run/AB12CD34.sock", User: "alice"}

func lines(conf string) []string { return strings.Split(strings.TrimSpace(conf), "\n") }

func TestBuildWritesTypedFieldsAndAllowlistedExtras(t *testing.T) {
	conf, err := Build(Profile{
		Remote: "vpn.example.test", Port: 1195, UserPass: true, MTU: 1400,
		Routes: []string{"10.20.0.0/16", "fd00:1::/64"},
		Extra:  "client\ndev tun\nproto udp\ncipher AES-256-GCM\nverb 3\nremote-cert-eku \"TLS Web Server Authentication\"\n<ca>\n" + pem + "\n</ca>\n<tls-crypt>\n-----BEGIN OpenVPN Static key V1-----\n0123abcd\n-----END OpenVPN Static key V1-----\n</tls-crypt>",
		Key:    "-----BEGIN PRIVATE KEY-----\nMIIEvQ\n-----END PRIVATE KEY-----",
	}, unixMgmt)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"proto udp", "cipher AES-256-GCM", "verb 3", "remote-cert-eku \"TLS Web Server Authentication\"",
		"dev tun", "remote vpn.example.test 1195", "tun-mtu 1400",
		"route 10.20.0.0 255.255.0.0", "route-ipv6 fd00:1::/64",
		`management "` + unixMgmt.Socket + `" unix`, "management-client-user alice", "management-hold", "management-query-passwords",
		"auth-user-pass", "auth-retry none",
		"<ca>", "</ca>", "<tls-crypt>", "<key>", "-----BEGIN PRIVATE KEY-----",
	} {
		if !strings.Contains(conf, want+"\n") {
			t.Errorf("config lacks %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "redirect-gateway") {
		t.Errorf("a split profile redirects the gateway:\n%s", conf)
	}
	// Each extra directive appears once, and the typed lines come after them, so they win.
	ls := lines(conf)
	idx := func(s string) int {
		for i, l := range ls {
			if l == s {
				return i
			}
		}
		return -1
	}
	if idx("cipher AES-256-GCM") > idx("management-hold") {
		t.Errorf("typed lines do not come last:\n%s", conf)
	}
	if n := strings.Count(conf, "dev tun\n"); n != 1 {
		t.Errorf("dev written %d times:\n%s", n, conf)
	}
}

// A full tunnel is redirect-gateway def1, never a default route line.
func TestBuildFullTunnelRedirectsTheGateway(t *testing.T) {
	conf, err := Build(Profile{Remote: "203.0.113.9", Routes: []string{"0.0.0.0/0", "::/0", "10.1.0.0/24"}, Extra: caBlock}, unixMgmt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, "redirect-gateway def1 ipv6\n") || strings.Contains(conf, "route 0.0.0.0") || strings.Contains(conf, "route-ipv6 ::/0") {
		t.Errorf("full tunnel:\n%s", conf)
	}
	if !strings.Contains(conf, "remote 203.0.113.9 1194\n") || !strings.Contains(conf, "route 10.1.0.0 255.255.255.0\n") {
		t.Errorf("defaults:\n%s", conf)
	}
}

// KTD8: file- and script-bearing directives, and anything off the
// allowlist, refuse the whole profile; nothing is silently dropped.
func TestBuildRefusesScriptAndFileDirectives(t *testing.T) {
	for _, extra := range []string{
		"up /tmp/x.sh", "down /tmp/x.sh", "script-security 2", "plugin /tmp/evil.so", "log /etc/passwd", "log-append /tmp/l",
		"writepid /etc/cron.d/x", "status /tmp/s", "route-up /bin/sh", "ipchange /bin/sh", "client-connect /bin/sh",
		"tls-verify /bin/sh", "auth-user-pass-verify /bin/sh via-env", "learn-address /bin/sh", "cd /", "chroot /",
		"daemon", "--up /bin/sh", "management 0.0.0.0 7505", "config /tmp/other.ovpn", "ca /etc/ssl/ca.pem",
		"auth-user-pass /tmp/creds", "verb 5", "dev tap", "remote evil.example 1194", "cipher AES\\x", "frobnicate yes",
		"<ca>\n" + pem + "\n</ca>\nup /bin/sh\n<ca>\nx\n</ca>", // a smuggled close tag inside a block body
		"<ca>\n-----BEGIN-----\n</ca>up /bin/sh\n</ca>",
		"<connection>\nremote evil 1\n</connection>", "<ca>\nno end",
	} {
		if conf, err := Build(Profile{Remote: "vpn.example.test", Extra: extra}, unixMgmt); err == nil {
			t.Errorf("%q accepted:\n%s", extra, conf)
		}
	}
}

func TestBuildRefusesBadTypedFields(t *testing.T) {
	for name, p := range map[string]Profile{
		"no remote":     {},
		"option remote": {Remote: "-vpn"},
		"spaced remote": {Remote: "vpn example"},
		"newline":       {Remote: "vpn\nup /bin/sh"},
		"port":          {Remote: "vpn", Port: 70000},
		"mtu":           {Remote: "vpn", MTU: 100},
		"route":         {Remote: "vpn", Routes: []string{"10.0.0.0/33"}},
		"key block":     {Remote: "vpn", Key: "x\n</key>\nup /bin/sh"},
	} {
		if _, err := Build(p, unixMgmt); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := Build(Profile{Remote: "vpn"}, Management{Socket: "/run/x.sock", User: "a b"}); err == nil {
		t.Error("a client user with a space accepted")
	}
}

// Windows: the management interface listens on loopback with a password file.
func TestBuildWindowsManagement(t *testing.T) {
	conf, err := Build(Profile{Remote: "vpn", Extra: caBlock}, Management{Addr: "127.0.0.1:50123", PassFile: `C:\Program Files\HostbayNetd\run\AB.pw`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, `management 127.0.0.1 50123 "C:/Program Files/HostbayNetd/run/AB.pw"`+"\n") || strings.Contains(conf, "management-client-user") {
		t.Errorf("windows management:\n%s", conf)
	}
}

// Every directive the .ovpn import keeps is one Build accepts, so an
// imported profile connects.
func TestBuildAcceptsWhatImportKeeps(t *testing.T) {
	for _, name := range []string{"office.ovpn", "full.ovpn"} {
		res, err := vpnconf.Parse(filepath.Join("../../../testdata/importers/vpn", name), "")
		if err != nil || len(res.Bundle.VPNs) != 1 {
			t.Fatalf("%s: %v %+v", name, err, res.Skipped)
		}
		v := res.Bundle.VPNs[0]
		var sec store.VPNSecrets
		if err := json.Unmarshal([]byte(v.Secret), &sec); err != nil {
			t.Fatal(err)
		}
		extra := sec.OpenVPN
		if name == "full.ovpn" {
			extra += "\npeer-fingerprint " + fingerprint // its ca is a file reference, which import drops
		}
		conf, err := Build(Profile{Remote: v.Gateway, Port: v.Port, Routes: v.Routes, MTU: v.MTU, UserPass: v.Auth == "password",
			Extra: extra, Key: sec.PrivateKey}, unixMgmt)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if name == "office.ovpn" && !strings.Contains(conf, "<ca>") {
			t.Errorf("%s: no CA:\n%s", name, conf)
		}
	}
}

// A refusal names the line, never its text: a key line whose block lost
// its open tag would otherwise reach the UI.
func TestRefusalNeverQuotesKeyMaterial(t *testing.T) {
	for _, keyLine := range []string{"0123456789abcdef0123456789abcdef", "ab12cd34ef56ab12cd34ef56ab12cd34"} {
		_, err := Build(Profile{Remote: "vpn", Extra: "client\n" + keyLine + "\n</tls-auth>"}, unixMgmt)
		if err == nil || strings.Contains(err.Error(), keyLine[:6]) || !strings.Contains(err.Error(), "line 2") {
			t.Fatalf("err %v", err)
		}
	}
	if _, err := Build(Profile{Remote: "vpn", Extra: "plugin /x.so"}, unixMgmt); err == nil || !strings.Contains(err.Error(), "(plugin)") {
		t.Fatalf("err %v", err)
	}
}

// OpenVPN stops at its options error "You must define CA file" and exits
// 1 before it opens the management socket; a hand-made profile with only
// a server, user and password reached that, and the run showed only
// "exit status 1". Build refuses it up front, naming the cause.
func TestBuildRefusesAProfileOpenVPNCannotTrust(t *testing.T) {
	_, err := Build(Profile{Remote: "203.0.113.9", UserPass: true, Routes: []string{"0.0.0.0/0", "::/0"}}, unixMgmt)
	if err == nil || !strings.Contains(err.Error(), "no CA certificate or peer fingerprint") {
		t.Fatalf("a profile with no CA built: %v", err)
	}
	for _, extra := range []string{caBlock, "peer-fingerprint " + fingerprint} {
		if _, err := Build(Profile{Remote: "203.0.113.9", Extra: extra}, unixMgmt); err != nil {
			t.Errorf("Extra %.20q: %v", extra, err)
		}
	}
}
