package vpnconf

import (
	"encoding/xml"
	"errors"
	"strings"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// anyconnect turns each HostEntry of an AnyConnect client profile into an
// app-scoped profile. Split-tunnel lists come from the gateway at connect,
// not from this file.
func anyconnect(b *importer.Builder, raw []byte) error {
	var doc struct {
		XMLName xml.Name `xml:"AnyConnectProfile"`
		Hosts   []struct {
			Name    string `xml:"HostName"`
			Address string `xml:"HostAddress"`
			Group   string `xml:"UserGroup"`
		} `xml:"ServerList>HostEntry"`
	}
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return errors.New("this is not an AnyConnect profile: " + err.Error())
	}
	for _, h := range doc.Hosts {
		addr := strings.TrimSpace(h.Address)
		if i := strings.Index(addr, "://"); i >= 0 {
			addr = addr[i+3:]
		}
		var ignored []string
		if host, path, ok := strings.Cut(addr, "/"); ok {
			addr = host
			ignored = append(ignored, "HostAddress path /"+path)
		}
		p := newProfile(strings.TrimSpace(h.Name), "anyconnect", store.VPNTierApp)
		p.Gateway, p.Port = hostPort(addr, 443)
		p.AuthGroup = strings.TrimSpace(h.Group)
		if p.Name == "" {
			p.Name = p.Gateway
		}
		p.Auth = "password"
		b.AddVPN(importer.VPN{Profile: p, Ignored: ignored})
	}
	return nil
}
