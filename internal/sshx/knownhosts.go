package sshx

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"hostbay/internal/apperr"
)

// HostKeyError is a hop's host key that known_hosts does not vouch for.
// With no Known fingerprints the key is unknown: the app turns it into a
// trust prompt with HostKeyTrust.Request(Host, Port, KeyType, Key,
// Fingerprint). With Known fingerprints the key changed and the connection is
// refused with no accept path (R13).
//
// errors.As(err, *apperr.Error) also works: it yields HostKeyUnknown or
// HostKeyChanged with hop, host, port, keyType and fingerprint details (and
// knownFingerprints for a change), so the code survives any wrapping.
type HostKeyError struct {
	Hop         int    // index into Target.Hops
	Host        string // known_hosts name: the hop's HostKeyAlias, else its Host
	Port        int
	KeyType     string
	Key         []byte // ssh wire-format public key
	Fingerprint string // SHA256:... of Key
	Known       []string
}

func (e *HostKeyError) Changed() bool { return len(e.Known) > 0 }

func (e *HostKeyError) Error() string { return e.AppError().Message }

func (e *HostKeyError) Unwrap() error { return e.AppError() }

// AppError is the UI form of e. An unknown key's error has no requestId; the
// app gets one from HostKeyTrust.Request.
func (e *HostKeyError) AppError() *apperr.Error {
	where := fmt.Sprintf("%s port %d", e.Host, e.Port)
	if e.Hop > 0 {
		where += fmt.Sprintf(" (jump hop %d)", e.Hop)
	}
	var ae *apperr.Error
	if e.Changed() {
		ae = apperr.New(apperr.HostKeyChanged, "The host key for "+where+" has changed. The connection was refused.").
			With("knownFingerprints", strings.Join(e.Known, ","))
	} else {
		ae = apperr.New(apperr.HostKeyUnknown, "The authenticity of "+where+" cannot be established")
	}
	return ae.With("hop", strconv.Itoa(e.Hop)).
		With("host", e.Host).
		With("port", strconv.Itoa(e.Port)).
		With("keyType", e.KeyType).
		With("fingerprint", e.Fingerprint)
}

// DefaultKnownHostsPath is ~/.ssh/known_hosts.
func DefaultKnownHostsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

// probeKey never matches a stored key; checking it lists the stored ones.
var probeKey = func() ssh.PublicKey {
	k, _ := ssh.NewPublicKey(ed25519.PublicKey(make([]byte, ed25519.PublicKeySize)))
	return k
}()

// hostKeyCallback checks hop's key for name:port against the known_hosts
// file at path (a missing file has no entries). It also returns the host key
// algorithms to offer: those of the stored keys first, so a host known by
// its ed25519 key that also has RSA is not mistaken for a changed key (AE10),
// then the rest, so a host that dropped its stored key type is refused as a
// change with both fingerprints instead of failing negotiation.
func hostKeyCallback(path string, hop int, name string, port int) (ssh.HostKeyCallback, []string, error) {
	db, err := knownhosts.New(path)
	if errors.Is(err, os.ErrNotExist) {
		db, err = knownhosts.New()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read known_hosts: %w", err)
	}
	addr := net.JoinHostPort(name, strconv.Itoa(port))
	remote := &net.TCPAddr{IP: net.IPv4zero, Port: port} // unused: addr takes precedence

	var algos []string
	var ke *knownhosts.KeyError
	if errors.As(db(addr, remote, probeKey), &ke) {
		for _, w := range ke.Want {
			for _, a := range algorithmsFor(w.Key.Type()) {
				if !slices.Contains(algos, a) {
					algos = append(algos, a)
				}
			}
		}
	}
	for _, a := range ssh.SupportedAlgorithms().HostKeys {
		if !slices.Contains(algos, a) {
			algos = append(algos, a)
		}
	}

	cb := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		err := db(addr, remote, key)
		var ke *knownhosts.KeyError
		if !errors.As(err, &ke) {
			return err // nil, or a revoked key
		}
		hk := &HostKeyError{Hop: hop, Host: name, Port: port, KeyType: key.Type(), Key: key.Marshal(), Fingerprint: ssh.FingerprintSHA256(key)}
		for _, w := range ke.Want {
			hk.Known = append(hk.Known, ssh.FingerprintSHA256(w.Key))
		}
		return hk
	}
	return cb, algos, nil
}

// algorithmsFor maps a stored key type to the host key algorithms that
// present it. RSA uses the SHA-2 variants only.
func algorithmsFor(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{keyType}
}

// validHostName refuses anything that could break or widen a known_hosts
// line: whitespace, control characters, separators, patterns and markers.
func validHostName(host string) error {
	if host == "" || len(host) > 253 {
		return errors.New("invalid host name for known_hosts")
	}
	for _, r := range host {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune(",*?!|#@[]\\\"", r) {
			return fmt.Errorf("host name %q cannot be written to known_hosts", host)
		}
	}
	return nil
}

// NewKnownHostAppender returns a function with the shape app.NewHostKeyTrust
// takes that appends an accepted key to the known_hosts file at path, as
// "host" on port 22 and "[host]:port" otherwise. It creates the directory
// (0700) and file (0600) if needed and appends in one O_APPEND write, so a
// line OpenSSH appends at the same moment is not lost.
func NewKnownHostAppender(path string) func(host string, port int, keyType string, key []byte) error {
	return func(host string, port int, keyType string, key []byte) error {
		if err := validHostName(host); err != nil {
			return err
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("invalid port %d", port)
		}
		pub, err := ssh.ParsePublicKey(key)
		if err != nil {
			return fmt.Errorf("invalid host key: %w", err)
		}
		if pub.Type() != keyType {
			return fmt.Errorf("host key is %s, not %s", pub.Type(), keyType)
		}
		line := knownhosts.Line([]string{knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))}, pub) + "\n"

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		// A last line without a newline would merge with ours.
		if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
			last := make([]byte, 1)
			if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
				line = "\n" + line
			}
		}
		if _, err := f.WriteString(line); err != nil {
			return err
		}
		return f.Close()
	}
}

// RemoveKnownHost deletes every known_hosts line for host:port, plain or
// hashed, so a changed key can be accepted on the next connect (R13). It
// runs only when typedHost equals host and gate (OS user verification, e.g.
// secrets.Gate) passes. It re-reads the file, follows a symlink to its
// target and keeps the file's mode.
func RemoveKnownHost(ctx context.Context, path, host string, port int, typedHost string, gate func(ctx context.Context, reason string) error) error {
	if typedHost != host {
		return errors.New("the typed host name does not match")
	}
	if gate == nil {
		return apperr.New(apperr.VerifyFailed, "user verification is not available")
	}
	if err := gate(ctx, "Remove the saved host key for "+host); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return err
	}
	entry := knownhosts.Normalize(net.JoinHostPort(host, strconv.Itoa(port)))
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if !lineNames(sc.Text(), entry) {
			out.WriteString(sc.Text())
			out.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(resolved), ".known_hosts-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), resolved)
}

// lineNames reports whether a plain (unmarked) known_hosts line lists entry
// literally or as a |1| hash. Marker lines and patterns are left alone.
func lineNames(line, entry string) bool {
	fields := strings.Fields(line)
	if len(fields) < 3 || strings.HasPrefix(fields[0], "#") || strings.HasPrefix(fields[0], "@") {
		return false
	}
	for _, h := range strings.Split(fields[0], ",") {
		if h == entry {
			return true
		}
		if salt, sum, ok := strings.Cut(strings.TrimPrefix(h, "|1|"), "|"); ok && strings.HasPrefix(h, "|1|") {
			s, err1 := base64.StdEncoding.DecodeString(salt)
			want, err2 := base64.StdEncoding.DecodeString(sum)
			if err1 != nil || err2 != nil {
				continue
			}
			mac := hmac.New(sha1.New, s)
			mac.Write([]byte(entry))
			if hmac.Equal(mac.Sum(nil), want) {
				return true
			}
		}
	}
	return false
}

// KnownHost is one name in known_hosts, for Settings. Host and Port are what
// RemoveKnownHost takes; a hashed name hides both, so Hashed entries are
// listed but cannot be picked for removal.
type KnownHost struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Hashed      bool   `json:"hashed"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
}

// ListKnownHosts lists every name of every plain line in the known_hosts
// file at path, in file order. Marker lines (@cert-authority, @revoked) and
// lines it cannot read are left out; a missing file has no entries.
func ListKnownHosts(path string) ([]KnownHost, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []KnownHost{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []KnownHost{}
	// One line at a time, so a line it cannot read hides only itself.
	for _, line := range bytes.Split(data, []byte("\n")) {
		marker, hosts, key, _, _, err := ssh.ParseKnownHosts(line)
		if err != nil || marker != "" {
			continue // a blank or comment line (io.EOF), an unreadable one, or a marker
		}
		for _, h := range hosts {
			e := KnownHost{Host: h, Port: 22, KeyType: key.Type(), Fingerprint: ssh.FingerprintSHA256(key)}
			if strings.HasPrefix(h, "|1|") {
				e = KnownHost{Hashed: true, KeyType: e.KeyType, Fingerprint: e.Fingerprint}
			} else if strings.HasPrefix(h, "[") {
				if host, port, err := net.SplitHostPort(h); err == nil {
					e.Host = host
					e.Port, _ = strconv.Atoi(port)
				}
			}
			out = append(out, e)
		}
	}
	return out, nil
}
