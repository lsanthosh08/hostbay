// Package xshell imports Xshell sessions from an exported .xts archive (a
// zip of .xsh files) or from a single .xsh file.
//
// The archive walk, folder paths and .xsh fields follow uniterm's
// backend/importer/xshell.go (github.com/ys-ll/uniterm), Copyright 2025
// uniTerm, licensed under the Apache License, Version 2.0. Hostbay adds the
// archive size caps, password decryption where the export alone allows it,
// proxies and X11.
package xshell

import (
	"archive/zip"
	"bytes"
	"crypto/md5"
	"crypto/rc4"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// Archive caps: a real export holds small text files.
const (
	maxEntries   = 10_000
	maxEntrySize = 1 << 20
	maxTotalSize = 64 << 20
)

// Parse reads the .xts or .xsh file at path. password is Xshell's master
// password, which Xshell 5.1 and later need to open saved passwords.
func Parse(p, password string) (importer.Result, error) {
	raw, err := importer.ReadFile(p)
	if err != nil {
		return importer.Result{}, err
	}
	b := importer.NewBuilder("Xshell")
	if !bytes.HasPrefix(raw, []byte("PK")) {
		name := path.Base(strings.ReplaceAll(p, `\`, "/"))
		session(b, nil, strings.TrimSuffix(name, path.Ext(name)), raw, password)
		return b.Result(), nil
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return importer.Result{}, fmt.Errorf("this is not an Xshell session export: %w", err)
	}
	if len(zr.File) > maxEntries {
		return importer.Result{}, fmt.Errorf("the archive holds more than %d files, too many for a session export", maxEntries)
	}
	// Check every size before unpacking anything.
	var total uint64
	for _, f := range zr.File {
		if f.UncompressedSize64 > maxEntrySize {
			return importer.Result{}, fmt.Errorf("the archive entry %q is larger than %d KiB, too large for a session file", importer.Text([]byte(f.Name)), maxEntrySize>>10)
		}
		total += f.UncompressedSize64
	}
	if total > maxTotalSize {
		return importer.Result{}, fmt.Errorf("the archive unpacks to more than %d MiB, too large for a session export", maxTotalSize>>20)
	}
	for _, f := range zr.File {
		// Chinese-locale Xshell writes entry names in GBK without the UTF-8
		// flag; Text keeps them readable where it can.
		name := strings.ReplaceAll(importer.Text([]byte(f.Name)), `\`, "/")
		if !strings.EqualFold(path.Ext(name), ".xsh") || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			b.Skip(name, err.Error())
			continue
		}
		// The header's size can lie; never read past the cap.
		data, err := io.ReadAll(io.LimitReader(rc, maxEntrySize+1))
		rc.Close()
		if err != nil || len(data) > maxEntrySize {
			b.Skip(name, "the session file could not be read within the size cap")
			continue
		}
		// The folder is the entry's directory below Xshell/Sessions/; ".."
		// and empty segments name no folder.
		dir := strings.TrimPrefix(strings.TrimPrefix(path.Dir(name), "Xshell/Sessions"), "Xshell")
		var folder []string
		for _, seg := range strings.Split(dir, "/") {
			if seg != "" && seg != "." && seg != ".." {
				folder = append(folder, seg)
			}
		}
		base := path.Base(name)
		session(b, folder, strings.TrimSuffix(base, path.Ext(base)), data, password)
	}
	return b.Result(), nil
}

// session adds one .xsh file's host, or skips it.
func session(b *importer.Builder, folder []string, name string, raw []byte, master string) {
	full := strings.Join(append(append([]string{}, folder...), name), "/")
	ini := importer.ParseINI(importer.Text(raw))
	proto := strings.ToUpper(ini.Get("CONNECTION", "Protocol"))
	port, _ := strconv.Atoi(ini.Get("CONNECTION", "Port"))
	h := store.Host{Alias: name, Hostname: ini.Get("CONNECTION", "Host"), Port: port, User: ini.Get("CONNECTION:AUTHENTICATION", "UserName")}
	switch proto {
	case "SSH", "SFTP":
		if h.Port == 0 {
			h.Port = 22
		}
		h.AuthType = new("password")
		if k := ini.Get("CONNECTION:AUTHENTICATION", "UserKey"); k != "" {
			// A name in Xshell's own key manager, not a file.
			b.Note(full, fmt.Sprintf("uses Xshell user key %q; add that key to Hostbay and choose it in its settings", k))
		}
		if ini.Get("CONNECTION:SSH", "ForwardX11") == "1" {
			h.X11 = new(true)
		}
	case "TELNET":
		h.Kind, h.User = new("telnet"), ""
		if h.Port == 0 {
			h.Port = 23
		}
	case "SERIAL":
		b.Skip(full, "serial ports are opened from Hostbay's Serial port action, not saved as hosts")
		return
	case "":
		b.Skip(full, "not an Xshell session file")
		return
	default:
		b.Skip(full, fmt.Sprintf("Xshell protocol %q has no Hostbay equivalent", proto))
		return
	}
	if px := ini.Get("CONNECTION:PROXY", "Proxy"); px != "" {
		h.ProxyID = importer.MissingProxy()
		b.Note(full, fmt.Sprintf("uses Xshell proxy %q, which was not imported; choose a proxy in its settings before connecting", px))
	}
	h.FolderID = b.Folder(folder)
	pw := ""
	if enc := ini.Get("CONNECTION:AUTHENTICATION", "Password"); enc != "" {
		var note string
		pw, note = password(enc, ini.Get("SessionInfo", "Version"), master)
		if note != "" {
			b.Note(full, note)
		}
	}
	b.Add(importer.Entry{Name: full, Host: h, Password: pw})
}

// password decrypts a saved password where the export alone allows it.
// Xshell before 5.1 used RC4 under MD5 of a fixed string. Later versions
// use RC4 under SHA-256 of the master password when one is set, and
// otherwise of the Windows account's SID, which the export does not carry;
// those append SHA-256 of the password as a check.
func password(enc, version, master string) (string, string) {
	ct, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(ct) == 0 {
		return "", "its saved password is damaged and was not imported"
	}
	if older(version, 5, 1) {
		k := md5.Sum([]byte("!X@s#h$e%l^l&"))
		if pw := rc4Open(k[:], ct); valid(pw) {
			return pw, ""
		}
		return "", "its saved password could not be decrypted and was not imported"
	}
	if master == "" {
		return "", "its saved password was not imported: Xshell ties it to your Windows account unless a master password is set; enter the master password, or the password after import"
	}
	if len(ct) <= sha256.Size {
		return "", "its saved password is damaged and was not imported"
	}
	k := sha256.Sum256([]byte(master))
	body, sum := ct[:len(ct)-sha256.Size], ct[len(ct)-sha256.Size:]
	pw := rc4Open(k[:], body)
	if got := sha256.Sum256([]byte(pw)); !bytes.Equal(got[:], sum) || !valid(pw) {
		return "", "its saved password did not open with that master password and was not imported"
	}
	return pw, ""
}

func rc4Open(key, ct []byte) string {
	c, _ := rc4.NewCipher(key) // MD5 and SHA-256 keys are valid lengths
	out := make([]byte, len(ct))
	c.XORKeyStream(out, ct)
	return string(out)
}

func valid(s string) bool {
	return s != "" && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

// older reports a "major.minor" version below major.minor; an unreadable
// one counts as current.
func older(v string, major, minor int) bool {
	a, b, _ := strings.Cut(strings.TrimSpace(v), ".")
	ma, err := strconv.Atoi(a)
	if err != nil {
		return false
	}
	mi, _ := strconv.Atoi(b)
	return ma < major || (ma == major && mi < minor)
}
