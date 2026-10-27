// Package mobaxterm imports MobaXterm sessions from an exported
// .mxtsessions file or from MobaXterm.ini, which also holds saved passwords.
//
// The bookmark line layout (#icon#type%host%port%user%…, SubRep folders)
// follows uniterm's backend/importer/mobaxterm.go (github.com/ys-ll/uniterm),
// Copyright 2025 uniTerm, licensed under the Apache License, Version 2.0.
// Hostbay reads the session type from its number rather than the icon, and
// adds the master-password decryption of MobaXterm.ini's [Passwords].
package mobaxterm

import (
	"crypto/aes"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// types names MobaXterm's session types by the number that opens a
// bookmark's settings, in the order its New session dialog lists them.
var types = []string{"SSH", "Telnet", "Rsh", "Xdmcp", "RDP", "VNC", "FTP", "SFTP", "Serial", "File", "Shell", "Browser", "Mosh", "AWS S3", "WSL"}

// Parse reads the file at path. password is MobaXterm's master password,
// used only for the saved passwords in MobaXterm.ini.
func Parse(path, password string) (importer.Result, error) {
	raw, err := importer.ReadFile(path)
	if err != nil {
		return importer.Result{}, err
	}
	lines := strings.Split(importer.Text(raw), "\n")
	b := importer.NewBuilder("MobaXterm")
	saved := passwords(importer.ParseINI(importer.Text(raw)), password)
	if saved.note != "" {
		b.Note("MobaXterm.ini", saved.note)
	}

	section, folder, found := "", []string(nil), false
	for _, line := range lines {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section, folder = strings.ToLower(strings.Trim(line, "[]")), nil
			continue
		}
		if section != "bookmarks" && !strings.HasPrefix(section, "bookmarks_") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		switch {
		case name == "SubRep":
			folder = strings.FieldsFunc(value, func(r rune) bool { return r == '\\' || r == '/' })
			continue
		case name == "ImgNum" || !strings.HasPrefix(value, "#"):
			continue
		}
		found = true
		full := strings.Join(append(append([]string{}, folder...), name), "/")
		parts := strings.Split(value, "#")
		if len(parts) < 3 {
			b.Skip(full, "the session settings are incomplete")
			continue
		}
		f := strings.Split(parts[2], "%")
		h, reason := toHost(name, f, b, full)
		if reason != "" {
			b.Skip(full, reason)
			continue
		}
		h.FolderID = b.Folder(folder)
		pw := saved.lookup(h)
		if pw != "" && h.KindName() == "ssh" && h.IdentityFile == "" {
			h.AuthType = new("password")
		}
		b.Add(importer.Entry{Name: full, Host: h, Password: pw})
	}
	if !found {
		return importer.Result{}, fmt.Errorf("no MobaXterm sessions found; choose an exported .mxtsessions file or MobaXterm.ini")
	}
	return b.Result(), nil
}

// toHost maps one bookmark's %-separated settings, or says why not.
func toHost(name string, f []string, b *importer.Builder, full string) (store.Host, string) {
	field := func(i int) string {
		if i < len(f) && f[i] != "-1" {
			return strings.TrimSpace(f[i])
		}
		return ""
	}
	t, err := strconv.Atoi(field(0))
	if err != nil || t < 0 || t >= len(types) {
		return store.Host{}, fmt.Sprintf("unknown MobaXterm session type %q", field(0))
	}
	port, _ := strconv.Atoi(field(2))
	if port < 0 || port > 65535 {
		port = 0
	}
	h := store.Host{Alias: name, Hostname: field(1), User: field(3)}
	def := func(p int) int {
		if port == 0 {
			return p
		}
		return port
	}
	switch types[t] {
	case "SSH":
		h.Port, h.AuthType = def(22), new("password")
		if k := field(14); k != "" && k != "<none>" {
			if p := importer.KeyPath(k); p != "" {
				h.IdentityFile, h.AuthType = p, new("key")
			} else {
				b.Note(full, fmt.Sprintf("its key file %q is not a path on this computer; choose the key in its settings", k))
			}
		}
		// MobaXterm turns X11 forwarding on by default; Hostbay keeps it off
		// unless chosen (R47), so the default is not carried over.
		if len(f) > 5 && f[5] == "-1" {
			b.Note(full, "MobaXterm's X11 forwarding was not imported; turn it on in its settings if it needs it")
		}
		if g := field(8); g != "" {
			b.Note(full, fmt.Sprintf("its SSH gateway %s was not imported, so it connects directly; set a jump host before connecting", g))
		}
	case "SFTP":
		h.Port, h.AuthType = def(22), new("password")
	case "Mosh":
		h.Kind, h.Port, h.AuthType = new("mosh"), def(22), new("password")
	case "Telnet":
		h.Kind, h.Port, h.User = new("telnet"), def(23), ""
	case "RDP":
		h.Kind, h.AuthType, h.Port, h.RdpPort = new("rdp"), new("password"), 22, new(def(3389))
	case "VNC":
		h.Kind, h.Port, h.User = new("vnc"), port, ""
	case "FTP":
		// MobaXterm's FTP is usually plain; Hostbay asks for TLS unless told
		// otherwise, so it never sends the password in clear by surprise.
		h.Kind, h.Port = new("ftp"), port
		b.Note(full, "imported to require TLS (explicit FTPS); set Security to None in its settings if the server has no TLS")
	case "Serial":
		return h, "serial ports are opened from Hostbay's Serial port action, not saved as hosts"
	default:
		return h, fmt.Sprintf("MobaXterm %s sessions have no Hostbay equivalent", types[t])
	}
	return h, ""
}

// saved holds MobaXterm.ini's decrypted passwords by "user@host".
type saved struct {
	byLogin map[string]string
	note    string
}

func (s saved) lookup(h store.Host) string {
	if h.User == "" {
		return ""
	}
	return s.byLogin[strings.ToLower(h.User+"@"+h.Hostname)]
}

// passwords decrypts MobaXterm.ini's [Passwords] with the master password.
// Their names are "user@host", optionally after a "protocol:" prefix.
// Without a master password, MobaXterm ties them to the Windows computer,
// so they cannot be read here.
func passwords(ini importer.INI, master string) saved {
	out := saved{byLogin: map[string]string{}}
	entries := ini["passwords"]
	if len(entries) == 0 {
		return out
	}
	if master == "" {
		out.note = "saved passwords were not imported: enter your MobaXterm master password to import them (without one, MobaXterm ties them to the Windows computer)"
		return out
	}
	bad := 0
	for name, enc := range entries {
		pw, ok := decrypt(enc, master)
		if !ok {
			bad++
			continue
		}
		if _, login, found := strings.Cut(name, ":"); found {
			name = login
		}
		out.byLogin[strings.ToLower(name)] = pw
	}
	if bad > 0 {
		out.note = fmt.Sprintf("%d saved password(s) could not be decrypted with that master password and were not imported", bad)
	}
	return out
}

// decrypt opens one MobaXterm master-password value: base64 of AES-256 in
// CFB-8 mode, keyed by the first 32 bytes of SHA-512(master password), with
// the IV the encryption of a zero block under that key. CFB-8 has no
// integrity check, so a result that is not one line of text is refused as
// the sign of a wrong master password.
func decrypt(enc, master string) (string, bool) {
	ct, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil || len(ct) == 0 {
		return "", false
	}
	sum := sha512.Sum512([]byte(master))
	block, _ := aes.NewCipher(sum[:32]) // a 32-byte key never fails
	var reg [aes.BlockSize]byte
	block.Encrypt(reg[:], reg[:])
	plain := make([]byte, len(ct))
	var out [aes.BlockSize]byte
	for i, c := range ct {
		block.Encrypt(out[:], reg[:])
		plain[i] = c ^ out[0]
		copy(reg[:], reg[1:])
		reg[aes.BlockSize-1] = c
	}
	s := string(plain)
	if !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", false
	}
	return s, true
}
