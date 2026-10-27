// Package virtviewer imports a virt-viewer .vv connection file, the INI
// file hypervisor consoles hand out for one SPICE or VNC display.
//
// Which .vv types import and which do not follows uniterm's
// backend/importer/virtviewer.go (github.com/ys-ll/uniterm), Copyright 2025
// uniTerm, licensed under the Apache License, Version 2.0. Unlike uniterm,
// Hostbay connects to a SPICE tls-port over TLS, so it prefers that port.
package virtviewer

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// Parse reads the .vv file at path. It holds one display; its password is
// usually a short-lived ticket.
func Parse(path, _ string) (importer.Result, error) {
	raw, err := importer.ReadFile(path)
	if err != nil {
		return importer.Result{}, err
	}
	ini := importer.ParseINI(importer.Text(raw))
	get := func(k string) string { return ini.Get("virt-viewer", k) }
	name := get("title")
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	b := importer.NewBuilder("virt-viewer")
	typ := strings.ToLower(get("type"))
	switch typ {
	case "spice", "vnc":
	case "":
		return importer.Result{}, errors.New("this is not a virt-viewer file: it has no [virt-viewer] type")
	case "ovirt":
		b.Skip(name, "an oVirt file finds its display through the oVirt API, which Hostbay does not use")
		return b.Result(), nil
	default:
		b.Skip(name, fmt.Sprintf("virt-viewer type %q has no Hostbay equivalent", typ))
		return b.Result(), nil
	}
	if get("host") == "" {
		if get("unix-path") != "" {
			b.Skip(name, "it connects through a local socket, not a host and port")
		} else {
			b.Skip(name, "no host name")
		}
		return b.Result(), nil
	}
	port := func(k string) int {
		p, err := strconv.Atoi(get(k))
		if err != nil || p < 1 || p > 65535 {
			return 0
		}
		return p
	}
	h := store.Host{Alias: name, Hostname: get("host"), User: get("username"), Kind: &typ}
	switch tls, plain := port("tls-port"), port("port"); {
	case typ == "spice" && tls != 0:
		h.Port = tls
		if get("ca") != "" {
			b.Note(name, "the file's CA certificate was not imported; Hostbay asks you to trust the server's certificate on first connect")
		}
	case typ == "spice":
		h.Port, h.TLS = plain, new("none")
	default:
		h.Port = plain
		b.Note(name, "imported to require VeNCrypt encryption; set Security to None in its settings if the server offers plain VNC only")
	}
	if get("proxy") != "" {
		h.ProxyID = importer.MissingProxy()
		b.Note(name, "uses a proxy, which was not imported; choose a proxy in its settings before connecting")
	}
	b.Add(importer.Entry{Name: name, Host: h, Password: get("password")})
	return b.Result(), nil
}
