package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx/proxytest"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
)

const kubeToken = "kube-Token-77!"

// apiServer is a fake Kubernetes API server that counts connections and
// records the client certificates presented.
type apiServer struct {
	*httptest.Server
	conns  atomic.Int32
	closed atomic.Int32

	mu    sync.Mutex
	certs []string // client certificate common names, per request
}

func newAPIServer(t *testing.T, cert *tls.Certificate, h http.HandlerFunc) *apiServer {
	t.Helper()
	a := &apiServer{}
	a.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		for _, c := range r.TLS.PeerCertificates {
			a.certs = append(a.certs, c.Subject.CommonName)
		}
		a.mu.Unlock()
		h(w, r)
	}))
	a.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			a.conns.Add(1)
		case http.StateClosed:
			a.closed.Add(1)
		}
	}
	a.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	if cert != nil {
		a.TLS.Certificates = []tls.Certificate{*cert}
	}
	a.StartTLS()
	t.Cleanup(a.Close)
	return a
}

func (a *apiServer) port() int { return a.Listener.Addr().(*net.TCPAddr).Port }

func (a *apiServer) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.Certificate().Raw}))
}

// versionOK answers /version when the token is sent, and refuses the
// rest with a message that echoes the token.
func versionOK(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+kubeToken {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "message": "Unauthorized"})
		return
	}
	if r.URL.Path == "/version" {
		json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v1.31.0"})
		return
	}
	w.WriteHeader(http.StatusForbidden)
	json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "message": "token " + kubeToken + " may not list pods"})
}

func newKubernetesService(t *testing.T, knownHosts ...string) (*KubernetesService, *ConnService, *HostService) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	c, s, dir := connWithKnownHosts(t, knownHosts...)
	return NewKubernetesService(c.creds, c, tlstrust.New(dir)), c, s
}

// importKubeconfig writes a kubeconfig for one context and imports it.
func importKubeconfig(t *testing.T, s *HostService, name, cluster, user string) Host {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	text := "clusters:\n- name: c\n  cluster:\n" + cluster + "users:\n- name: u\n  user:\n" + user +
		"contexts:\n- name: " + name + "\n  context: {cluster: c, user: u, namespace: apps}\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportFrom("kubeconfig", path, ""); err != nil {
		t.Fatal(err)
	}
	hosts, _ := s.ListHosts()
	for _, h := range hosts {
		if h.Alias == name {
			return h
		}
	}
	t.Fatalf("%s was not imported", name)
	return Host{}
}

func indent(k, v string) string { return "    " + k + ": " + v + "\n" }

// Without CA data the server's self-signed certificate is pinned after
// the user accepts it (insecure-skip-tls-verify is never honoured), and a
// changed one is refused with a removal request; no token is sent until
// the certificate is trusted.
func TestKubernetesConnectPinsAndRefusesAChangedCertificate(t *testing.T) {
	k, _, s := newKubernetesService(t)
	srv := newAPIServer(t, nil, versionOK)
	h := importKubeconfig(t, s, "lab", indent("server", srv.URL)+indent("insecure-skip-tls-verify", "true"), indent("token", kubeToken))
	ctx := context.Background()
	_, err := k.KubernetesConnect(ctx, h.ID)
	var ae *apperr.Error
	if !errorsAs(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "kubernetesCert" {
		t.Fatalf("err = %v", err)
	}
	if err := k.KubernetesTrustCert(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	info, err := k.KubernetesConnect(ctx, h.ID)
	if err != nil || info.Namespace != "apps" || info.Context != "lab" || info.Version != "v1.31.0" {
		t.Fatalf("connect = %+v, %v", info, err)
	}
	// The same address now serves another certificate.
	port := srv.port()
	srv.Close()
	other := tlstest.Cert(t)
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Skip("the port was taken before the second server could listen:", err)
	}
	evil := httptest.NewUnstartedServer(http.HandlerFunc(versionOK))
	evil.Listener.Close()
	evil.Listener = ln
	evil.TLS = &tls.Config{Certificates: []tls.Certificate{other}}
	var sawToken atomic.Bool
	evil.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sawToken.Store(r.Header.Get("Authorization") != "") })
	evil.StartTLS()
	defer evil.Close()
	_, err = k.KubernetesConnect(ctx, h.ID)
	if !errorsAs(err, &ae) || ae.Code != apperr.HostKeyChanged || ae.Details["removeId"] == "" {
		t.Fatalf("err = %v", err)
	}
	if sawToken.Load() {
		t.Fatal("the token was sent to a changed certificate")
	}
}

// With CA data, that CA is the only anchor: the server it vouches for
// connects under the TLS server name, with no prompt, and errors that echo
// the token reach the page without it.
func TestKubernetesCADataIsTheOnlyAnchorAndErrorsHideTheToken(t *testing.T) {
	k, _, s := newKubernetesService(t)
	srv := newAPIServer(t, nil, versionOK)
	h := importKubeconfig(t, s, "prod", indent("server", "https://localhost:"+strconv.Itoa(srv.port()))+
		indent("certificate-authority-data", base64.StdEncoding.EncodeToString([]byte(srv.caPEM())))+indent("tls-server-name", "127.0.0.1"), indent("token", kubeToken))
	ctx := context.Background()
	info, err := k.KubernetesConnect(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = k.KubernetesList(ctx, info.ClusterID, "pods", "apps", "")
	if err == nil || strings.Contains(err.Error(), kubeToken) || !strings.Contains(err.Error(), "may not list pods") {
		t.Fatalf("err = %v", err)
	}
	otherCert := tlstest.Cert(t) // httptest servers all share one certificate
	other := newAPIServer(t, &otherCert, versionOK)
	h2 := importKubeconfig(t, s, "wrong-ca", indent("server", other.URL)+
		indent("certificate-authority-data", base64.StdEncoding.EncodeToString([]byte(srv.caPEM()))), indent("token", kubeToken))
	_, err = k.KubernetesConnect(ctx, h2.ID)
	if err == nil || apperr.CodeOf(err) == apperr.HostKeyUnknown {
		t.Fatalf("a server the CA did not sign: %v", err)
	}
}

// A cluster behind a jump host is dialled by the jump host; with the
// bastion down the connect fails at the jump step and the API server sees
// no connection. A proxy-url cluster has the missing-proxy marker and
// does not connect at all (KTD24, KTD28).
func TestKubernetesNeverDialsAroundItsJumpHostOrProxy(t *testing.T) {
	srv := newAPIServer(t, nil, versionOK)
	jump := sftptest.New(t, "jump-pw")
	jump.Resolve("k8s.internal", srv.Listener.Addr().String())
	k, _, s := newKubernetesService(t, jump.KnownHostsLine())
	jhost, jport, _ := net.SplitHostPort(jump.Addr)
	jp, _ := strconv.Atoi(jport)
	auth, jpw := "password", "jump-pw"
	j, err := s.SaveHost(Host{Alias: "bastion", Hostname: jhost, Port: jp, User: "u", AuthType: &auth, Password: &jpw})
	if err != nil {
		t.Fatal(err)
	}
	h := importKubeconfig(t, s, "inner", indent("server", "https://k8s.internal:"+strconv.Itoa(srv.port()))+
		indent("certificate-authority-data", base64.StdEncoding.EncodeToString([]byte(srv.caPEM())))+indent("tls-server-name", "127.0.0.1"), indent("token", kubeToken))
	h.JumpHostID = &j.ID
	if _, err := s.SaveHost(h); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := k.KubernetesConnect(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if want := net.JoinHostPort("k8s.internal", strconv.Itoa(srv.port())); !slices.Contains(jump.Dialed(), want) {
		t.Fatalf("jump host dialled %v, want %s", jump.Dialed(), want)
	}
	// The down case also has a cluster at an address this computer reaches
	// directly, so a direct fallback would show as an API connection.
	down, port := proxytest.Down(t)
	j.Hostname, j.Port, j.Password = down, port, &jpw // a moved host keeps no sealed password
	if _, err := s.SaveHost(j); err != nil {
		t.Fatal(err)
	}
	r := importKubeconfig(t, s, "reachable", indent("server", srv.URL)+
		indent("certificate-authority-data", base64.StdEncoding.EncodeToString([]byte(srv.caPEM()))), indent("token", kubeToken))
	r.JumpHostID = &j.ID
	if _, err := s.SaveHost(r); err != nil {
		t.Fatal(err)
	}
	before := srv.conns.Load()
	for _, id := range []string{h.ID, r.ID} {
		if _, err := k.KubernetesConnect(ctx, id); err == nil {
			t.Fatal("connected with the jump host down")
		}
	}
	if srv.conns.Load() != before {
		t.Fatal("the API server saw a direct connection")
	}

	px := importKubeconfig(t, s, "proxied", indent("server", srv.URL)+indent("proxy-url", "socks5://proxy.example:1080"), indent("token", kubeToken))
	before = srv.conns.Load()
	if _, err := k.KubernetesConnect(ctx, px.ID); apperr.CodeOf(err) != apperr.ProxyFailed {
		t.Fatalf("err = %v", err)
	}
	if srv.conns.Load() != before {
		t.Fatal("a proxy-url cluster connected directly")
	}
}

type clientPKI struct {
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
}

func newClientPKI(t *testing.T) clientPKI {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "client ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return clientPKI{ca: ca, caKey: key}
}

// issue returns a client certificate for cn and its key, as kubeconfig
// user lines.
func (p clientPKI) issue(t *testing.T, cn string) string {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	enc := func(typ string, b []byte) string {
		return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}))
	}
	return indent("client-certificate-data", enc("CERTIFICATE", der)) + indent("client-key-data", enc("EC PRIVATE KEY", kb))
}

// A client-certificate cluster presents the key-store certificate, and a
// re-import with a renewed one updates the same host so the next connect
// presents the new certificate (KTD28).
func TestKubernetesClientCertificateAndReimport(t *testing.T) {
	k, _, s := newKubernetesService(t)
	pki := newClientPKI(t)
	srv := newAPIServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v1.31.0"})
	})
	cluster := indent("server", srv.URL) + indent("certificate-authority-data", base64.StdEncoding.EncodeToString([]byte(srv.caPEM())))
	h := importKubeconfig(t, s, "certs", cluster, pki.issue(t, "admin-old"))
	ctx := context.Background()
	info, err := k.KubernetesConnect(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	k.KubernetesClose(info.ClusterID)
	importKubeconfig(t, s, "certs", cluster, pki.issue(t, "admin-new"))
	hosts, _ := s.ListHosts()
	if len(hosts) != 1 || hosts[0].ID != h.ID {
		t.Fatalf("hosts = %+v", hosts)
	}
	if _, err := k.KubernetesConnect(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.certs) != 2 || srv.certs[0] != "admin-old" || srv.certs[1] != "admin-new" {
		t.Fatalf("client certificates presented: %v", srv.certs)
	}
}

// A re-import that changes a cluster's certificate authority but brings no
// token of its own drops the saved token: it is never sent to a server
// the new anchor vouches for without being asked for again.
func TestKubernetesReimportWithANewCADropsTheSavedToken(t *testing.T) {
	_, _, s := newKubernetesService(t)
	srv := newAPIServer(t, nil, versionOK)
	otherCert := tlstest.Cert(t)
	other := newAPIServer(t, &otherCert, versionOK)
	ca := func(a *apiServer) string {
		return indent("server", srv.URL) + indent("certificate-authority-data", base64.StdEncoding.EncodeToString([]byte(a.caPEM())))
	}
	h := importKubeconfig(t, s, "ctx", ca(srv), indent("token", kubeToken))
	if !h.HasPassword {
		t.Fatal("the first import saved no token")
	}
	importKubeconfig(t, s, "ctx", ca(srv), "")
	if hosts, _ := s.ListHosts(); len(hosts) != 1 || !hosts[0].HasPassword {
		t.Fatalf("a re-import with the same CA lost the token: %+v", hosts)
	}
	importKubeconfig(t, s, "ctx", ca(other), "")
	if hosts, _ := s.ListHosts(); len(hosts) != 1 || hosts[0].HasPassword {
		t.Fatalf("a re-import with a new CA kept the token: %+v", hosts)
	}
}

// Logs and exec open in the shared terminal registry under the cluster's
// ID, so closing the cluster closes them; a bad pod name is refused.
func TestKubernetesLogsCloseWithTheCluster(t *testing.T) {
	k, c, s := newKubernetesService(t)
	srv := newAPIServer(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v1.31.0"})
			return
		}
		w.Write([]byte("hello\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	h := importKubeconfig(t, s, "logs", indent("server", srv.URL)+indent("certificate-authority-data", base64.StdEncoding.EncodeToString([]byte(srv.caPEM()))), "    {}\n")
	ctx := context.Background()
	info, err := k.KubernetesConnect(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.KubernetesExec(ctx, info.ClusterID, "apps", "a;rm -rf /", "web", []string{"sh"}, 80, 24); err == nil {
		t.Fatal("the pod name a;rm -rf / was accepted")
	}
	id, err := k.KubernetesLogs(ctx, info.ClusterID, "apps", "web-1", "web")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PtyAttach(id); err != nil {
		t.Fatal(err)
	}
	if err := k.KubernetesClose(info.ClusterID); err != nil {
		t.Fatal(err)
	}
	if err := c.PtyAttach(id); err == nil {
		t.Fatal("the log stream outlived its cluster")
	}
	if _, err := k.KubernetesList(ctx, info.ClusterID, "pods", "apps", ""); err == nil {
		t.Fatal("a closed cluster still answered")
	}
}

// Closing a cluster tab closes its pooled API connections, so none keeps
// the jump host's lease after the tab is gone.
func TestKubernetesCloseClosesIdleConnections(t *testing.T) {
	k, _, s := newKubernetesService(t)
	srv := newAPIServer(t, nil, versionOK)
	h := importKubeconfig(t, s, "idle", indent("server", srv.URL)+indent("certificate-authority-data", base64.StdEncoding.EncodeToString([]byte(srv.caPEM()))), indent("token", kubeToken))
	info, err := k.KubernetesConnect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if srv.conns.Load() == 0 || srv.closed.Load() == srv.conns.Load() {
		t.Fatalf("opened %d, closed %d", srv.conns.Load(), srv.closed.Load())
	}
	k.KubernetesClose(info.ClusterID)
	for deadline := time.Now().Add(5 * time.Second); srv.closed.Load() < srv.conns.Load(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the idle API connection stayed open after the tab closed")
		}
	}
}

// A connect the API server refuses closes the connection it opened, so a
// stale token never leaves one, or the jump host's lease, held.
func TestKubernetesFailedConnectClosesItsConnection(t *testing.T) {
	k, _, s := newKubernetesService(t)
	srv := newAPIServer(t, nil, versionOK)
	h := importKubeconfig(t, s, "stale", indent("server", srv.URL)+indent("certificate-authority-data", base64.StdEncoding.EncodeToString([]byte(srv.caPEM()))), indent("token", "stale-token"))
	if _, err := k.KubernetesConnect(context.Background(), h.ID); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("err = %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); srv.conns.Load() == 0 || srv.closed.Load() < srv.conns.Load(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("opened %d, closed %d: the refused connect left its connection open", srv.conns.Load(), srv.closed.Load())
		}
	}
}

func errorsAs(err error, target **apperr.Error) bool {
	e, ok := err.(*apperr.Error)
	if ok {
		*target = e
	}
	return ok
}
