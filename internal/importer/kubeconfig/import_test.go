package kubeconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hostbay/internal/importer/importertest"
	"hostbay/internal/store"
)

var fixture = filepath.Join(importertest.Fixtures, "kubeconfig", "config")

// opened records every file the importer reads besides the kubeconfig.
func opened(t *testing.T) *[]string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var paths []string
	was := readRef
	readRef = func(p string) ([]byte, error) { paths = append(paths, p); return was(p) }
	t.Cleanup(func() { readRef = was })
	return &paths
}

func TestImportRefusesPluginsTokenFilesPasswordsAndPlainHTTP(t *testing.T) {
	paths := opened(t)
	st, v, dir := importertest.Store(t)
	res, err := Parse(fixture, "")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	if rep.Hosts != 2 {
		t.Fatalf("report = %+v", rep)
	}
	for name, why := range map[string]string{
		"eks-prod": "exec plugin", "gke": "auth provider", "from-file": "token file", "basic-auth": "user name and password",
		"insecure-port": "https", "impersonate": "impersonat", "dangling": "cluster",
	} {
		if r := importertest.Skipped(rep, name); !strings.Contains(r, why) {
			t.Errorf("%s: skip reason %q, want %q", name, r, why)
		}
	}
	if len(*paths) != 0 {
		t.Fatalf("files were opened: %v", *paths)
	}
	lab := importertest.Host(t, st, "lab")
	if lab.KindName() != "kubernetes" || store.Deref(lab.URL) != "https://lab.example:6443" || lab.Hostname != "lab.example" || lab.Port != 6443 ||
		store.Deref(lab.Namespace) != "apps" || lab.CAData != nil || importertest.Password(t, v, lab) != "fake-kube-token-lab-1" {
		t.Fatalf("lab = %+v", lab)
	}
	if !importertest.Noted(rep, "lab", "insecure-skip-tls-verify") {
		t.Errorf("the dropped skip-verify is not noted: %v", rep.Notes)
	}
	proxied := importertest.Host(t, st, "proxied")
	if !proxied.Proxied() || !importertest.Noted(rep, "proxied", "proxy") {
		t.Fatalf("proxied = %+v, notes %v", proxied, rep.Notes)
	}
	d, _ := st.Data()
	if d.Proxy(*proxied.ProxyID) != nil {
		t.Fatal("the missing-proxy marker names a saved proxy")
	}
	importertest.NoSecrets(t, dir, "fake-kube-token-lab-1", "fake-basic-password", "fake-kube-token-as-1")
}

type pki struct {
	caPEM, certPEM, keyPEM, otherKeyPEM []byte
}

func newPKI(t *testing.T, cn string) pki {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keyPEM := func(k *ecdsa.PrivateKey) []byte {
		b, _ := x509.MarshalECPrivateKey(k)
		return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
	}
	return pki{
		caPEM:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		certPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:      keyPEM(key),
		otherKeyPEM: keyPEM(other),
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// certConfig is a kubeconfig whose user signs in with p's client
// certificate, inline, and trusts p's CA.
func certConfig(t *testing.T, p pki) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	text := `apiVersion: v1
kind: Config
clusters:
- name: prod
  cluster:
    server: https://k8s.prod.example:6443
    certificate-authority-data: ` + b64(p.caPEM) + `
    tls-server-name: kubernetes.default
users:
- name: admin
  user:
    client-certificate-data: ` + b64(p.certPEM) + `
    client-key-data: ` + b64(p.keyPEM) + `
contexts:
- name: prod
  context: {cluster: prod, user: admin}
`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// keyOf opens the key-store entry host signs in with.
func keyOf(t *testing.T, st *store.Store, h store.Host, open func(string, []byte) ([]byte, error)) string {
	t.Helper()
	d, _ := st.Data()
	name, ok := strings.CutPrefix(h.IdentityFile, store.KeyRefPrefix)
	k := d.KeyByName(name)
	if !ok || k == nil {
		t.Fatalf("no key-store key for %q", h.IdentityFile)
	}
	raw, err := open(k.Data, store.KeyAAD(k.ID))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A renewed client certificate refreshes the host it was imported as: no
// second host, and its key-store entry holds the new certificate (KTD28).
func TestReimportRefreshesTheMatchingCluster(t *testing.T) {
	opened(t)
	st, v, dir := importertest.Store(t)
	first := newPKI(t, "admin")
	res, err := Parse(certConfig(t, first), "")
	if err != nil {
		t.Fatal(err)
	}
	if rep := importertest.Import(t, st, v, res); rep.Hosts != 1 || rep.Keys != 1 {
		t.Fatalf("report = %+v", rep)
	}
	h := importertest.Host(t, st, "prod")
	if store.Deref(h.CAData) != string(first.caPEM) || store.Deref(h.TLSServerName) != "kubernetes.default" {
		t.Fatalf("prod = %+v", h)
	}
	if k := keyOf(t, st, h, v.Open); !strings.Contains(k, string(first.certPEM)) || !strings.Contains(k, string(first.keyPEM)) {
		t.Fatal("the key store does not hold the client certificate and key")
	}

	renewed := newPKI(t, "admin")
	res, err = Parse(certConfig(t, renewed), "")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	if rep.Hosts != 0 || !importertest.Noted(rep, "prod", "refreshed") {
		t.Fatalf("report = %+v", rep)
	}
	after := importertest.Host(t, st, "prod")
	if after.ID != h.ID || store.Deref(after.CAData) != string(renewed.caPEM) {
		t.Fatalf("prod = %+v", after)
	}
	if k := keyOf(t, st, after, v.Open); !strings.Contains(k, string(renewed.certPEM)) || strings.Contains(k, string(first.certPEM)) {
		t.Fatal("the next connect would not use the renewed certificate")
	}
	d, _ := st.Data()
	if len(d.Keys.Items) != 1 {
		t.Fatalf("the old key stayed in the key store: %v", d.KeyNames())
	}
	importertest.NoSecrets(t, dir, string(first.keyPEM), string(renewed.keyPEM))
}

// A client-key file that is not the certificate's key is refused, and
// nothing of it is stored. Files are read once, capped at 64 KiB.
func TestClientKeyFileMustMatchItsCertificate(t *testing.T) {
	paths := opened(t)
	st, v, dir := importertest.Store(t)
	p := newPKI(t, "admin")
	folder := t.TempDir()
	os.WriteFile(filepath.Join(folder, "admin.crt"), p.certPEM, 0o600)
	os.WriteFile(filepath.Join(folder, "wrong.key"), p.otherKeyPEM, 0o600)
	os.WriteFile(filepath.Join(folder, "big.crt"), append(p.certPEM, make([]byte, 64<<10)...), 0o600)
	cfg := filepath.Join(folder, "config")
	os.WriteFile(cfg, []byte(`clusters:
- name: c
  cluster: {server: "https://k8s.example"}
users:
- name: wrong
  user: {client-certificate: admin.crt, client-key: wrong.key}
- name: big
  user: {client-certificate: big.crt, client-key-data: `+b64(p.keyPEM)+`}
contexts:
- name: wrong-key
  context: {cluster: c, user: wrong}
- name: too-big
  context: {cluster: c, user: big}
`), 0o600)
	res, err := Parse(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	rep := importertest.Import(t, st, v, res)
	if rep.Hosts != 0 || rep.Keys != 0 || !strings.Contains(importertest.Skipped(rep, "wrong-key"), "key") || !strings.Contains(importertest.Skipped(rep, "too-big"), "64 KiB") {
		t.Fatalf("report = %+v", rep)
	}
	if strings.Join(*paths, ",") != filepath.Join(folder, "admin.crt")+","+filepath.Join(folder, "wrong.key")+","+filepath.Join(folder, "big.crt") {
		t.Fatalf("read %v", *paths)
	}
	importertest.NoSecrets(t, dir, string(p.otherKeyPEM), string(p.keyPEM))
}

func TestDefaultPathIsTheUsersKubeconfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if _, err := Parse("", ""); err == nil || !strings.Contains(err.Error(), filepath.Join(home, ".kube", "config")) {
		t.Fatalf("err = %v", err)
	}
}
