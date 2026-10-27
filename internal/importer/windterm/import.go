// Package windterm imports WindTerm's user.sessions, decrypting its saved
// logins with the master password when WindTerm has one set.
//
// The session model and the autoLogin decryption (PBKDF2-HMAC-SHA3-512 over
// the profile's fingerprint, AES-256-CBC) are adapted from uniterm's
// backend/importer/windterm.go (github.com/ys-ll/uniterm), Copyright 2025
// uniTerm, licensed under the Apache License, Version 2.0. Hostbay adds
// Telnet, key-file path checks, and does not import key passphrases, which
// it asks for at connect.
package windterm

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha3"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

// session is one entry of user.sessions, whose keys are flat dotted names.
type session struct {
	Protocol  string `json:"session.protocol"`
	Target    string `json:"session.target"`
	Port      *int   `json:"session.port"`
	Label     string `json:"session.label"`
	Group     string `json:"session.group"`
	AutoLogin string `json:"session.autoLogin"` // JSON, or base64 AES-256-CBC of it
	SSH       struct {
		IdentityFilePath string `json:"identityFilePath.windows"`
	} `json:"ssh"`
}

type autoLogin struct {
	PasswordEnabled bool   `json:"PasswordEnabled"`
	Password        string `json:"Password"`
	SessionUser     string `json:"session.user"`
	PublicKey       struct {
		WindowsPath string `json:"windows.path"`
		WindowsPass string `json:"windows.pass"`
	} `json:"Public Key"`
}

// Parse reads user.sessions at path. WindTerm keeps whether a master
// password is set, and the salt for it, in user.config one folder up.
func Parse(path, password string) (importer.Result, error) {
	raw, err := importer.ReadFile(path)
	if err != nil {
		return importer.Result{}, err
	}
	var sessions []session
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return importer.Result{}, errors.New("this is not a WindTerm user.sessions file")
	}
	key, err := masterKey(filepath.Join(filepath.Dir(filepath.Dir(path)), "user.config"), password)
	if err != nil {
		return importer.Result{}, err
	}
	b := importer.NewBuilder("WindTerm")
	for _, s := range sessions {
		folder := strings.FieldsFunc(s.Group, func(r rune) bool { return r == '>' })
		label := strings.TrimSpace(s.Label)
		host, user := target(s.Target)
		if label == "" {
			label = host
		}
		name := strings.Join(append(append([]string{}, folder...), label), "/")
		h := store.Host{Alias: label, Hostname: host, User: user}
		switch strings.ToUpper(s.Protocol) {
		case "SSH":
			h.Port = 22
		case "TELNET":
			h.Kind, h.Port, h.User = new("telnet"), 23, ""
		case "SERIAL":
			b.Skip(name, "serial ports are opened from Hostbay's Serial port action, not saved as hosts")
			continue
		default:
			b.Skip(name, fmt.Sprintf("WindTerm protocol %q has no Hostbay equivalent", s.Protocol))
			continue
		}
		if s.Port != nil {
			if *s.Port < 1 || *s.Port > 65535 {
				b.Skip(name, fmt.Sprintf("port %d is out of range", *s.Port))
				continue
			}
			h.Port = *s.Port
		}
		al, err := login(s.AutoLogin, key)
		if err != nil {
			b.Note(name, "its saved login could not be read and was not imported: "+err.Error())
		}
		pw := ""
		if h.KindName() == "ssh" {
			if al != nil && strings.TrimSpace(al.SessionUser) != "" {
				h.User = strings.TrimSpace(al.SessionUser)
			}
			keyFile := s.SSH.IdentityFilePath
			if al != nil && strings.TrimSpace(al.PublicKey.WindowsPath) != "" {
				keyFile = al.PublicKey.WindowsPath
			}
			switch {
			case al != nil && al.PasswordEnabled && al.Password != "":
				h.AuthType, pw = new("password"), al.Password
			case strings.TrimSpace(keyFile) != "":
				if p := importer.KeyPath(keyFile); p != "" {
					h.AuthType, h.IdentityFile = new("key"), p
				} else {
					h.AuthType = new("password")
					b.Note(name, fmt.Sprintf("its key file %q is not a path on this computer; choose the key in its settings", keyFile))
				}
				if al != nil && al.PublicKey.WindowsPass != "" {
					b.Note(name, "its key passphrase was not imported; Hostbay asks for it when connecting")
				}
			default:
				h.AuthType = new("agent")
			}
		}
		h.FolderID = b.Folder(folder)
		b.Add(importer.Entry{Name: name, Host: h, Password: pw})
	}
	return b.Result(), nil
}

// target splits "user@host" at its last "@"; with no user, root.
func target(t string) (host, user string) {
	t = strings.TrimSpace(t)
	if i := strings.LastIndex(t, "@"); i > 0 && i < len(t)-1 {
		return t[i+1:], t[:i]
	}
	return t, "root"
}

type cbcKey struct{ key, iv []byte }

// masterKey reads user.config and derives the autoLogin key: PBKDF2 with
// HMAC-SHA3-512, 100000 rounds, the profile fingerprint as salt, 48 bytes
// split into an AES-256 key and IV. It returns nil when no master password
// is set or there is no user.config, as autoLogin is then plain JSON.
func masterKey(configPath, master string) (*cbcKey, error) {
	raw, err := importer.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Fingerprint    string `json:"application.fingerprint"`
		MasterPassword bool   `json:"application.masterPassword"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, errors.New("WindTerm's user.config next to the sessions could not be read")
	}
	if !cfg.MasterPassword {
		return nil, nil
	}
	if cfg.Fingerprint == "" {
		return nil, errors.New("WindTerm's user.config has no fingerprint, so its saved logins cannot be decrypted")
	}
	if master == "" {
		return nil, errors.New("WindTerm has a master password set; enter it to import")
	}
	m, err := pbkdf2.Key(func() hash.Hash { return sha3.New512() }, master, []byte(cfg.Fingerprint), 100_000, 48)
	if err != nil {
		return nil, err
	}
	return &cbcKey{key: m[:32], iv: m[32:]}, nil
}

// login returns a session's autoLogin, decrypting it when it is not JSON.
func login(raw string, k *cbcKey) (*autoLogin, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var al autoLogin
	if json.Unmarshal([]byte(raw), &al) == nil {
		return &al, nil
	}
	if k == nil {
		return nil, errors.New("it is encrypted, but WindTerm's user.config was not found next to the sessions")
	}
	ct, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil, errors.New("it is damaged")
	}
	block, _ := aes.NewCipher(k.key)
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, k.iv).CryptBlocks(plain, ct)
	n, pad := len(plain), int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize {
		return nil, errors.New("the master password is wrong")
	}
	for _, c := range plain[n-pad:] {
		if int(c) != pad {
			return nil, errors.New("the master password is wrong")
		}
	}
	if json.Unmarshal(plain[:n-pad], &al) != nil {
		return nil, errors.New("the master password is wrong")
	}
	return &al, nil
}
