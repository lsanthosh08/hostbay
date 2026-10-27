// TestDefaultPasswordVector uses a vector from uniterm's
// backend/importer/mremoteng_crypto_test.go (github.com/ys-ll/uniterm),
// Copyright 2025 uniTerm, licensed under the Apache License, Version 2.0,
// which uniterm generated with Node's crypto rather than a Go decoder.

package mremoteng

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hostbay/internal/importer/importertest"
)

var fixture = filepath.Join(importertest.Fixtures, "mremoteng", "confCons.xml")

func TestImportMapsHostsFoldersAndSealsPasswords(t *testing.T) {
	st, v, dir := importertest.Store(t)
	res, err := Parse(fixture, "mr-master")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	if rep.Hosts != 5 || rep.Folders != 2 {
		t.Fatalf("report = %+v", rep)
	}
	db := importertest.Host(t, st, "db-1")
	if db.KindName() != "ssh" || db.User != "ops" || db.Port != 22 || importertest.Password(t, v, db) != "Ops-Pass-1!" {
		t.Errorf("db-1 = %+v", db)
	}
	if got := importertest.FolderPath(t, st, db); got != "Datacenter/Linux" {
		t.Errorf("db-1 folder = %q", got)
	}
	win := importertest.Host(t, st, "win-1")
	if win.KindName() != "rdp" || win.User != `CORP\alice` || *win.RdpPort != 3390 || *win.RdpSecurity != "nla" || importertest.Password(t, v, win) != "Win-Pass-2!" {
		t.Errorf("win-1 = %+v", win)
	}
	if vnc := importertest.Host(t, st, "vnc-1"); vnc.KindName() != "vnc" || importertest.Password(t, v, vnc) != "Vnc-Pass-3!" {
		t.Errorf("vnc-1 = %+v", vnc)
	}
	if raw := importertest.Host(t, st, "raw-svc"); raw.KindName() != "tcp" || raw.Port != 7000 {
		t.Errorf("raw-svc = %+v", raw)
	}
	if !importertest.Noted(rep, "router", "unencrypted") {
		t.Errorf("Telnet not noted as unencrypted: %v", rep.Notes)
	}
	for name, want := range map[string]string{
		"Datacenter/Linux/old-ssh": "SSH-1",
		"intranet":                 "no Hostbay equivalent",
		"evil":                     "not a valid address",
	} {
		if got := importertest.Skipped(rep, name); !strings.Contains(got, want) {
			t.Errorf("%s skipped for %q, want %q", name, got, want)
		}
	}
	importertest.NoSecrets(t, dir, "Ops-Pass-1!", "Win-Pass-2!", "Vnc-Pass-3!")

	res, _ = Parse(fixture, "mr-master")
	importertest.Reimport(t, st, v, res)
}

func TestWrongOrMissingMasterPasswordIsRefused(t *testing.T) {
	if _, err := Parse(fixture, ""); err == nil || !strings.Contains(err.Error(), "enter it") {
		t.Errorf("no password: %v", err)
	}
	if _, err := Parse(fixture, "wrong"); err == nil || !strings.Contains(err.Error(), "wrong") {
		t.Errorf("wrong password: %v", err)
	}
}

func TestDefaultPasswordVector(t *testing.T) {
	const ct = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh+J3sB/kCVgWKp6OFLyKAQRkwuQk3mUoCpl00MXK8RPBtIawhA="
	if pw, err := decrypt(ct, defaultPassword, 1000); err != nil || pw != "synthetic-secret-✓" {
		t.Fatalf("decrypt = %q, %v", pw, err)
	}
}

func TestHostileFiles(t *testing.T) {
	for name, body := range map[string]string{
		"full-file encryption": `<Connections EncryptionEngine="AES" BlockCipherMode="GCM" KdfIterations="1000" FullFileEncryption="true">c3ludGhldGlj</Connections>`,
		"bare base64":          `c3ludGhldGlj`,
		"external entity":      `<!DOCTYPE c [<!ENTITY x SYSTEM "file:///etc/passwd">]><Connections><Node Name="&x;" Type="Connection" Protocol="SSH2" Hostname="192.0.2.1"/></Connections>`,
	} {
		p := filepath.Join(t.TempDir(), "confCons.xml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(p, ""); err == nil {
			t.Errorf("%s: imported", name)
		} else if strings.Contains(err.Error(), "root:") {
			t.Errorf("%s: error leaks a file: %v", name, err)
		}
	}
	// Iterations past the cap leave passwords behind instead of spending
	// the work.
	p := filepath.Join(t.TempDir(), "confCons.xml")
	body := `<Connections EncryptionEngine="AES" BlockCipherMode="GCM" KdfIterations="5000000"><Node Name="a" Type="Connection" Protocol="SSH2" Hostname="192.0.2.1" Password="AAAA"/></Connections>`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := Parse(p, "")
	if err != nil || len(res.Bundle.Hosts) != 1 || res.Bundle.Hosts[0].Password != "" || len(res.Notes) != 1 {
		t.Fatalf("res = %+v, %v", res, err)
	}
}
