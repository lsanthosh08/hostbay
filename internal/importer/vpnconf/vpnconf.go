// Package vpnconf imports VPN profiles from the configuration files VPN
// clients use (R38): OpenVPN .ovpn, WireGuard .conf, AnyConnect profile
// .xml, Apple .mobileconfig, NetworkManager .nmconnection, Windows .pbk,
// and Tunnelblick .tblk and Viscosity .visc bundles. Each parser keeps the
// file's routes and DNS (R40), lists every directive it drops, such as a
// script hook (R43), and keeps only the secrets the file holds (R44).
package vpnconf

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// Parse reads the configuration file or bundle folder at path.
func Parse(path, _ string) (importer.Result, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return importer.Result{}, err
	}
	b := importer.NewBuilder("VPN")
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	ext := strings.ToLower(filepath.Ext(path))
	if fi.IsDir() {
		if _, err = os.Stat(filepath.Join(path, "config.conf")); err == nil {
			err = visc(b, path)
		} else {
			err = tblk(b, path, name)
		}
		if err != nil {
			return importer.Result{}, err
		}
		return b.Result(), nil
	}
	raw, err := importer.ReadFile(path)
	if err != nil {
		return importer.Result{}, err
	}
	text := importer.Text(raw)
	switch ext {
	case ".ovpn":
		ovpn(b, name, text, filepath.Dir(path))
	case ".conf":
		if strings.Contains(strings.ToLower(text), "[interface]") {
			e, err := wgEntry(name, text)
			if err != nil {
				return importer.Result{}, err
			}
			b.AddVPN(e)
		} else {
			ovpn(b, name, text, filepath.Dir(path))
		}
	case ".xml":
		err = anyconnect(b, raw)
	case ".mobileconfig":
		err = mobileconfig(b, raw)
	case ".nmconnection":
		err = nmconnection(b, name, text)
	case ".pbk":
		pbk(b, text)
	default:
		err = fmt.Errorf("%s is not a VPN configuration file Hostbay reads", filepath.Base(path))
	}
	if err != nil {
		return importer.Result{}, err
	}
	return b.Result(), nil
}

// fullTunnel is every address, as the profile editor's full tunnel.
var fullTunnel = []string{"0.0.0.0/0", "::/0"}

// kv is one key = value line, with the line as written for the report.
type kv struct{ key, value, line string }

// section is one [name] of an INI-style file, in order, keeping repeated
// keys (WireGuard repeats AllowedIPs) and the name's case.
type section struct {
	name string
	keys []kv
}

func (s section) get(key string) string {
	v := ""
	for _, k := range s.keys {
		if strings.EqualFold(k.key, key) {
			v = k.value
		}
	}
	return v
}

func (s section) all(key string) []string {
	var out []string
	for _, k := range s.keys {
		if strings.EqualFold(k.key, key) {
			out = append(out, k.value)
		}
	}
	return out
}

func parseINI(text string) []section {
	var out []section
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[' && strings.HasSuffix(line, "]"):
			out = append(out, section{name: strings.TrimSpace(line[1 : len(line)-1])})
		case len(out) > 0:
			if k, v, ok := strings.Cut(line, "="); ok {
				out[len(out)-1].keys = append(out[len(out)-1].keys, kv{strings.TrimSpace(k), strings.TrimSpace(v), line})
			}
		}
	}
	return out
}

func find(secs []section, name string) section {
	for _, s := range secs {
		if strings.EqualFold(s.name, name) {
			return s
		}
	}
	return section{}
}

// list splits a comma- or semicolon-separated value, dropping blanks.
func list(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' })
}

// hostPort splits "host", "host:port" or "[v6]:port"; port is def when
// absent and -1 when it is not a port.
func hostPort(s string, def int) (string, int) {
	s = strings.TrimSpace(s)
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return h, -1
		}
		return h, n
	}
	return strings.Trim(s, "[]"), def
}

// network reads a CIDR, or a bare address as one host, in canonical form.
func network(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return "", false
		}
		return netip.PrefixFrom(a, a.BitLen()).String(), true
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return "", false
	}
	return p.Masked().String(), true
}

// address reads an interface address, keeping its host bits.
func address(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		return network(s)
	}
	p, err := netip.ParsePrefix(s)
	return p.String(), err == nil
}

// dns splits resolvers from search domains: WireGuard and NetworkManager
// list both in one setting.
func dns(values []string) (servers, domains []string) {
	for _, v := range values {
		if _, err := netip.ParseAddr(v); err == nil {
			servers = append(servers, v)
		} else if v != "" {
			domains = append(domains, v)
		}
	}
	return servers, domains
}

// clip shortens a dropped line for the report.
func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// readRef reads a file a configuration names, only a bare file name beside
// it in dir (its own folder or bundle), so a crafted file cannot pull
// ~/.ssh keys or other files into a synced profile, even from a
// configuration saved in the home folder.
func readRef(dir, name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\:`) {
		return "", errors.New("not a file beside the configuration")
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	p, err := filepath.EvalSymlinks(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(root, p); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("outside the configuration's folder")
	}
	raw, err := importer.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(importer.Text(raw), "\r\n"), nil
}

func newProfile(name, typ, tier string) store.VPN {
	return store.VPN{Name: name, Type: typ, Tier: tier}
}
