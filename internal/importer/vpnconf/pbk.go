package vpnconf

import (
	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// pbk imports the VPN entries of a Windows phonebook (rasphone.pbk) as
// system-wide profiles. Windows keeps their passwords elsewhere, so each
// asks for its password on first connect.
func pbk(b *importer.Builder, text string) {
	for _, s := range parseINI(text) {
		if s.get("Type") != "2" {
			b.Skip(s.name, "not a VPN entry")
			continue
		}
		switch s.get("VpnStrategy") {
		case "1", "2":
			b.Skip(s.name, "PPTP is not supported: it is not secure")
			continue
		}
		p := newProfile(s.name, "ipsec", store.VPNTierSystem)
		p.Gateway, p.Auth = s.get("PhoneNumber"), "password"
		b.AddVPN(importer.VPN{Profile: p})
	}
}
