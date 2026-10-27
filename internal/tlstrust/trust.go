// Package tlstrust pins the TLS certificates of RDP, VeNCrypt VNC and
// SPICE TLS-port servers and of FTPS, WebDAV, S3, database and Kubernetes
// hosts and of AI providers (R28, KTD18, KTD25, KTD31): system roots
// first, then a self-signed certificate the user accepted by its
// fingerprint, and never a changed one. No setting skips verification.
package tlstrust

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"hostbay/internal/apperr"
)

// pendingTTL is how long the user has to answer a certificate prompt.
const pendingTTL = 5 * time.Minute

// labels names each kind's certificate in messages.
var labels = map[string]string{"rdp": "RDP", "ftp": "FTPS", "webdav": "WebDAV", "s3": "S3", "vnc": "VNC", "spice": "SPICE", "postgres": "PostgreSQL", "kubernetes": "Kubernetes", "redis": "Redis", "mongodb": "MongoDB", "elasticsearch": "Elasticsearch",
	"mysql": "MySQL", "mssql": "SQL Server", "oracle": "Oracle", "rqlite": "rqlite", "ai": "AI provider"}

// Store pins server certificates on first use. As with SSH host keys
// (app.HostKeyTrust), the UI only answers yes or no to an opaque request ID
// and never supplies certificate material.
//
// Each kind keeps its pins in its own file in Hostbay's data dir
// ("rdp-certs.json" and so on). Pins are keyed by host ID and the real
// target name and port, never by a local forward's port, so a host behind a
// jump chain keeps its pin.
type Store struct {
	dir string
	now func() time.Time

	mu       sync.Mutex
	pending  map[string]pendingCert
	removals map[string]pendingCert // changed pins the user may remove (KTD25); no fingerprint
}

type pendingCert struct {
	kind, key, fingerprint string
	host                   string // the refused host name a removal must be typed as
	expires                time.Time
}

type certPins struct {
	Version int               `json:"version"`
	Pins    map[string]string `json:"pins"` // pinKey -> Fingerprint
}

func New(dir string) *Store {
	return &Store{dir: dir, now: time.Now, pending: map[string]pendingCert{}, removals: map[string]pendingCert{}}
}

// Fingerprint is the SHA-256 of a DER certificate, in OpenSSH's format.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func pinKey(hostID, host string, port int) string {
	return hostID + "|" + net.JoinHostPort(host, strconv.Itoa(port))
}

func certFile(kind string) string { return kind + "-certs.json" }

// Check returns nil when der is the pinned certificate, a HostKeyUnknown
// prompt carrying a request ID when nothing is pinned yet, and HostKeyChanged
// naming both fingerprints when another certificate is pinned. Both errors
// carry details.kind "<kind>Cert", such as "rdpCert" or "ftpCert".
func (t *Store) Check(kind, hostID, host string, port int, der []byte) error {
	fp := Fingerprint(der)
	key := pinKey(hostID, host, port)
	t.mu.Lock()
	defer t.mu.Unlock()
	pins, err := t.load(kind)
	if err != nil {
		return err
	}
	target := net.JoinHostPort(host, strconv.Itoa(port))
	label := labels[kind]
	switch want, ok := pins.Pins[key]; {
	case ok && want == fp:
		return nil
	case ok:
		return apperr.New(apperr.HostKeyChanged, fmt.Sprintf(
			"The %s certificate of %s has changed, so Hostbay did not send your password. Expected %s, got %s. If the server's certificate was renewed, check the new fingerprint with its administrator.",
			label, target, want, fp)).
			With("kind", kind+"Cert").With("host", host).With("port", strconv.Itoa(port)).
			With("expected", want).With("fingerprint", fp)
	}
	id := t.add(t.pending, pendingCert{kind: kind, key: key, fingerprint: fp})
	return apperr.New(apperr.HostKeyUnknown, "The "+label+" certificate of "+target+" is not known yet").
		With("kind", kind+"Cert").With("host", host).With("port", strconv.Itoa(port)).
		With("keyType", label+" certificate").With("fingerprint", fp).With("requestId", id)
}

// add stores p in m under a new random ID that expires after pendingTTL;
// t.mu is held.
func (t *Store) add(m map[string]pendingCert, p pendingCert) string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	id := hex.EncodeToString(b[:])
	now := t.now()
	for k, old := range m { // prompts never answered would pile up
		if now.After(old.expires) {
			delete(m, k)
		}
	}
	p.expires = now.Add(pendingTTL)
	m[id] = p
	return id
}

// RemovalRequest records the pin a HostKeyChanged refusal named and returns
// the ID RemovePin takes, so the page never names the pin it removes.
func (t *Store) RemovalRequest(kind, hostID, host string, port int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.add(t.removals, pendingCert{kind: kind, key: pinKey(hostID, host, port), host: host})
}

// RemovePin deletes the pin behind removeID, so a server's new certificate
// can be trusted on the next connect. As with a changed host key (R13),
// typedHost must equal the refused host name and gate (OS user
// verification) must pass; otherwise the pin stays. A mistyped name or a
// cancelled verification can be tried again; a removal works once.
func (t *Store) RemovePin(ctx context.Context, removeID, typedHost string, gate func(ctx context.Context, reason string) error) error {
	t.mu.Lock()
	p, ok := t.removals[removeID]
	t.mu.Unlock()
	if !ok || t.now().After(p.expires) {
		return errors.New("this certificate refusal has expired; connect again")
	}
	if typedHost != p.host {
		return errors.New("the typed host name does not match")
	}
	if gate == nil {
		return apperr.New(apperr.VerifyFailed, "user verification is not available")
	}
	if err := gate(ctx, "Remove the saved "+labels[p.kind]+" certificate of "+p.host); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.removals[removeID]; !ok {
		return errors.New("this certificate refusal has expired; connect again")
	}
	pins, err := t.load(p.kind)
	if err != nil {
		return err
	}
	delete(pins.Pins, p.key)
	if err := t.save(p.kind, pins); err != nil {
		return err
	}
	delete(t.removals, removeID)
	return nil
}

// Answer resolves a pending prompt; accepting pins its certificate. Each ID
// works once; unknown and expired IDs are refused.
func (t *Store) Answer(requestID string, accept bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.pending[requestID]
	delete(t.pending, requestID)
	if !ok || t.now().After(p.expires) {
		return errors.New("this certificate prompt has expired; connect again")
	}
	if !accept {
		return nil
	}
	pins, err := t.load(p.kind)
	if err != nil {
		return err
	}
	pins.Pins[p.key] = p.fingerprint
	return t.save(p.kind, pins)
}

func (t *Store) save(kind string, pins certPins) error {
	b, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(t.dir, certFile(kind), b)
}

// Connect finds the TLS config a host's connections use. It runs try, which
// connects and signs in, first with the system roots. If the server's
// certificate does not verify there, it is checked against the pin (Check)
// and try runs again trusting only that certificate. try is never run
// without verification, so a refused certificate never sees a password.
func (t *Store) Connect(kind, hostID, host string, port int, try func(*tls.Config) error) (*tls.Config, error) {
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, ClientSessionCache: tls.NewLRUClientSessionCache(0)}
	err := try(cfg)
	var ve *tls.CertificateVerificationError
	if !errors.As(err, &ve) || len(ve.UnverifiedCertificates) == 0 {
		return cfg, err
	}
	leaf := ve.UnverifiedCertificates[0]
	if err := t.Check(kind, hostID, host, port, leaf.Raw); err != nil {
		return nil, err
	}
	pinned := t.pinnedOnly(cfg.Clone(), leaf)
	if err := try(pinned); err != nil {
		return nil, t.pinErr(kind, host, err)
	}
	return pinned, nil
}

// errPinExpired is the pinned certificate's own validity period ending.
var errPinExpired = errors.New("the pinned certificate has expired")

// pinnedOnly makes cfg accept exactly leaf and nothing else. The pin is
// those certificate bytes, and the handshake proves the server holds their
// key, so the name and chain checks that stand in for a pin elsewhere are
// not run: a server's self-made certificate rarely names the address it is
// reached by. A pinned CA certificate would also vouch for others it
// signed, so only the leaf itself is accepted, and only while it is valid.
func (t *Store) pinnedOnly(cfg *tls.Config, leaf *x509.Certificate) *tls.Config {
	cfg.InsecureSkipVerify = true
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, leaf.Raw) {
			return errors.New("the server sent a certificate other than the pinned one")
		}
		if now := t.now(); now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			return errPinExpired
		}
		return nil
	}
	return cfg
}

// pinErr names an expired pin; drivers may flatten errPinExpired to text.
func (t *Store) pinErr(kind, host string, err error) error {
	if errors.Is(err, errPinExpired) || strings.Contains(err.Error(), errPinExpired.Error()) {
		return fmt.Errorf("the pinned %s certificate of %s has expired; ask its administrator for the renewed one: %w", labels[kind], host, err)
	}
	return err
}

// Anchored is the TLS config for a server whose CA the user's own
// configuration names, as a kubeconfig's certificate-authority-data does
// (KTD28): that CA is the only trust anchor, the system roots are not
// consulted, and nothing is ever pinned, so a server it did not sign is
// refused outright. serverName is the name checked in the certificate.
func Anchored(caPEM []byte, serverName string) (*tls.Config, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the certificate authority data holds no certificate")
	}
	return &tls.Config{ServerName: serverName, RootCAs: roots, MinVersion: tls.VersionTLS12}, nil
}

// Capture is Connect for drivers that flatten TLS errors to text, so
// errors.As finds no leaf (KTD25). The leaf is recorded inside the TLS
// config instead: crypto/tls's own check is off, and VerifyConnection runs
// the full check in its place, against the system roots for host, then
// against exactly the pinned certificate. A driver that overwrites RootCAs
// therefore changes nothing. As with Connect, try never runs unverified,
// so a refused certificate never sees a sign-in packet. The returned config
// keeps checking on every later connection.
func (t *Store) Capture(kind, hostID, host string, port int, try func(*tls.Config) error) (*tls.Config, error) {
	var mu sync.Mutex
	var leaf *x509.Certificate
	first := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			err := verifyChain(cs, host, nil)
			if err != nil && len(cs.PeerCertificates) > 0 {
				mu.Lock()
				leaf = cs.PeerCertificates[0]
				mu.Unlock()
			}
			return err
		}}
	err := try(first)
	mu.Lock()
	unverified := leaf
	mu.Unlock()
	if unverified == nil {
		return first, err
	}
	if err := t.Check(kind, hostID, host, port, unverified.Raw); err != nil {
		return nil, err
	}
	pinned := t.pinnedOnly(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}, unverified)
	if err := try(pinned); err != nil {
		return nil, t.pinErr(kind, host, err)
	}
	return pinned, nil
}

// verifyChain is crypto/tls's server check: the chain the server sent must
// lead to roots (nil: the system's) and name host.
func verifyChain(cs tls.ConnectionState, host string, roots *x509.CertPool) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("the server sent no certificate")
	}
	opts := x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: x509.NewCertPool()}
	for _, c := range cs.PeerCertificates[1:] {
		opts.Intermediates.AddCert(c)
	}
	_, err := cs.PeerCertificates[0].Verify(opts)
	return err
}

func (t *Store) load(kind string) (certPins, error) {
	pins := certPins{Version: 1, Pins: map[string]string{}}
	b, err := os.ReadFile(filepath.Join(t.dir, certFile(kind)))
	if errors.Is(err, fs.ErrNotExist) {
		return pins, nil
	}
	if err != nil {
		return pins, err
	}
	// A damaged pin file fails closed: refusing beats re-trusting silently.
	if err := json.Unmarshal(b, &pins); err != nil {
		return pins, fmt.Errorf("%s is damaged: %w", certFile(kind), err)
	}
	if pins.Pins == nil {
		pins.Pins = map[string]string{}
	}
	return pins, nil
}

// writeFileAtomic replaces dir/name (mode 0600) by temp file and rename, as
// the store does, so a crash never leaves a half-written pin file.
func writeFileAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // fails harmlessly once renamed
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
}
