package vpnconf

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// mobileconfig imports the VPN payloads of an Apple configuration profile:
// IKEv2, L2TP and IPsec as system-wide profiles, a WireGuard payload as an
// app-scoped one, and any other VPN app's payload as an external profile.
func mobileconfig(b *importer.Builder, raw []byte) error {
	if bytes.HasPrefix(raw, []byte("bplist")) || len(raw) > 0 && raw[0] == 0x30 {
		return errors.New("this .mobileconfig is signed or binary, which Hostbay does not read; export it unsigned as XML")
	}
	root, err := plist(raw)
	if err != nil {
		return err
	}
	top, _ := root.(map[string]any)
	payloads, _ := top["PayloadContent"].([]any)
	for _, x := range payloads {
		m, _ := x.(map[string]any)
		if s(m, "PayloadType") != "com.apple.vpn.managed" {
			continue
		}
		name := s(m, "UserDefinedName")
		if name == "" {
			name = s(m, "PayloadDisplayName")
		}
		p := newProfile(name, "ipsec", store.VPNTierSystem)
		var sec store.VPNSecrets
		var ignored []string
		switch typ := s(m, "VPNType"); typ {
		case "IKEv2":
			ike := d(m, "IKEv2")
			p.Gateway, p.User = s(ike, "RemoteAddress"), s(ike, "AuthName")
			sec.Password, sec.PresharedKey = s(ike, "AuthPassword"), s(ike, "SharedSecret")
			switch {
			case s(ike, "ExtendedAuthEnabled") == "1" || s(ike, "ExtendedAuthEnabled") == "true":
				p.Auth = "password"
			case s(ike, "AuthenticationMethod") == "Certificate":
				p.Auth = "certificate"
			}
			if s(ike, "PayloadCertificateUUID") != "" {
				ignored = append(ignored, "PayloadCertificateUUID (the certificate payload is not imported)")
			}
		case "L2TP":
			ppp, ipsec := d(m, "PPP"), d(m, "IPSec")
			p.Gateway, p.User, p.Auth = s(ppp, "CommRemoteAddress"), s(ppp, "AuthName"), "password"
			sec.Password, sec.PresharedKey = s(ppp, "AuthPassword"), s(ipsec, "SharedSecret")
		case "IPSec":
			ipsec := d(m, "IPSec")
			p.Gateway, p.User, p.Auth = s(ipsec, "RemoteAddress"), s(ipsec, "XAuthName"), "password"
			sec.Password, sec.PresharedKey = s(ipsec, "XAuthPassword"), s(ipsec, "SharedSecret")
		default:
			sub := s(m, "VPNSubType")
			if wg := s(d(m, "VendorConfig"), "WgQuickConfig"); strings.HasPrefix(sub, "com.wireguard.") && wg != "" {
				e, err := wgEntry(name, wg)
				if err != nil {
					b.Skip(name, err.Error())
					continue
				}
				b.AddVPN(e)
				continue
			}
			// Another VPN app's payload: that app connects it.
			p = newProfile(name, "other", store.VPNTierExternal)
			p.Client = sub
			if p.Client == "" {
				p.Client = typ
			}
		}
		b.AddVPN(importer.VPN{Profile: p, Secrets: sec, Ignored: ignored})
	}
	return nil
}

func s(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return string(v)
	case bool:
		return fmt.Sprint(v)
	}
	return ""
}

func d(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

// plist decodes an XML property list: dict as map, array as slice, data as
// bytes, true and false as bool, and every other value as its text.
func plist(raw []byte) (any, error) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, errors.New("this is not a configuration profile: it holds no property list")
		}
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local != "plist" {
			return plistValue(dec, se)
		}
	}
}

func plistValue(dec *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "dict", "array":
		m, a, key := map[string]any{}, []any{}, ""
		for {
			tok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.EndElement:
				if start.Name.Local == "dict" {
					return m, nil
				}
				return a, nil
			case xml.StartElement:
				if t.Name.Local == "key" {
					if err := dec.DecodeElement(&key, &t); err != nil {
						return nil, err
					}
					continue
				}
				v, err := plistValue(dec, t)
				if err != nil {
					return nil, err
				}
				m[key] = v
				a = append(a, v)
			}
		}
	case "true", "false":
		return start.Name.Local == "true", dec.Skip()
	}
	var text string
	if err := dec.DecodeElement(&text, &start); err != nil {
		return nil, err
	}
	if start.Name.Local == "data" {
		return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(text), ""))
	}
	return text, nil
}
