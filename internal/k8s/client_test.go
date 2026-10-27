package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"hostbay/internal/tlstrust"
)

// fake is an API server over TLS that records what it was asked.
type fake struct {
	srv   *httptest.Server
	dials atomic.Int32

	mu   sync.Mutex
	reqs []*http.Request
	body map[string]string // "METHOD path" -> request body
}

func (f *fake) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		out = append(out, r.Method+" "+r.URL.Path)
	}
	return out
}

// newFake serves h and returns a client trusting only its certificate,
// dialling through a counted dial function as the host's would.
func newFake(t *testing.T, h http.HandlerFunc) (*fake, *Client) {
	t.Helper()
	f := &fake{body: map[string]string{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r)
		f.body[r.Method+" "+r.URL.Path] = string(b)
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	cfg, err := tlstrust.Anchored(ca, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(Config{URL: f.srv.URL, TLS: cfg, Token: "tok-123", Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		f.dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}})
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func TestNewRefusesAnythingButHTTPS(t *testing.T) {
	for _, u := range []string{"http://k8s.lan", "k8s.lan:6443", "https://user:pw@k8s.lan", "https://"} {
		if _, err := New(Config{URL: u, TLS: nil}); err == nil {
			t.Errorf("%s was accepted", u)
		}
	}
}

// HTTPS_PROXY in Hostbay's environment is never used: the request goes
// only through the host's dial function (KTD24, KTD26).
func TestEnvironmentProxyIsNeverUsed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var proxied atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			c.Close()
		}
	}()
	t.Setenv("HTTPS_PROXY", "http://"+ln.Addr().String())
	t.Setenv("HTTP_PROXY", "http://"+ln.Addr().String())
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]string{"gitVersion": "v1.31.0"})
	})
	v, err := c.Version(context.Background())
	if err != nil || v != "v1.31.0" {
		t.Fatalf("Version = %q, %v", v, err)
	}
	if proxied.Load() != 0 || f.dials.Load() != 1 {
		t.Fatalf("proxy saw %d connections, dial function %d", proxied.Load(), f.dials.Load())
	}
}

func TestRefusalCarriesTheServersMessage(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]any{"kind": "Status", "message": `pods is forbidden: User "dev" cannot list`, "reason": "Forbidden", "code": 403})
	})
	_, err := c.List(context.Background(), "pods", "apps", 0, "")
	if !IsStatus(err, 403) || !strings.Contains(err.Error(), "cannot list") {
		t.Fatalf("err = %v", err)
	}
}

func TestListPagesAndSummarizesPods(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"metadata": map[string]any{"continue": "next-page"},
			"items": []any{map[string]any{
				"metadata": map[string]any{"name": "web-1", "namespace": "apps", "creationTimestamp": "2026-10-01T00:00:00Z", "labels": map[string]string{"app": "web"}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "web"}, map[string]any{"name": "sidecar"}}},
				"status":   map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"ready": true, "restartCount": 2}, map[string]any{"ready": false, "restartCount": 1}}},
			}},
		})
	})
	p, err := c.List(context.Background(), "pods", "apps", 50, "page-2")
	if err != nil {
		t.Fatal(err)
	}
	r := f.reqs[0]
	if r.URL.Path != "/api/v1/namespaces/apps/pods" || r.URL.Query().Get("limit") != "50" || r.URL.Query().Get("continue") != "page-2" {
		t.Fatalf("asked %s?%s", r.URL.Path, r.URL.RawQuery)
	}
	if p.Continue != "next-page" || len(p.Items) != 1 {
		t.Fatalf("page = %+v", p)
	}
	it := p.Items[0]
	if it.Name != "web-1" || it.Namespace != "apps" || it.Status != "Running" || it.Ready != "1/2" || it.Restarts != 3 ||
		strings.Join(it.Containers, ",") != "web,sidecar" || it.Labels["app"] != "web" {
		t.Fatalf("item = %+v", it)
	}
	if _, err := c.List(context.Background(), "deployments", "", 0, ""); err != nil || f.reqs[1].URL.Path != "/apis/apps/v1/deployments" {
		t.Fatalf("all namespaces: %v %s", err, f.reqs[1].URL.Path)
	}
}

// Secrets reach the page as name, namespace, type, labels and key names
// only; a reveal returns one key's value (KTD28).
func TestSecretsShowKeyNamesOnlyAndRevealOneValue(t *testing.T) {
	secret := map[string]any{
		"kind": "Secret",
		"metadata": map[string]any{"name": "db", "namespace": "apps", "labels": map[string]string{"app": "db"},
			"annotations": map[string]string{"kubectl.kubernetes.io/last-applied-configuration": `{"stringData":{"password":"hunter2"}}`}},
		"type": "Opaque",
		"data": map[string]string{"password": base64.StdEncoding.EncodeToString([]byte("hunter2")), "user": base64.StdEncoding.EncodeToString([]byte("app"))},
	}
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/secrets") {
			writeJSON(w, map[string]any{"kind": "SecretList", "items": []any{secret}})
			return
		}
		writeJSON(w, secret)
	})
	ctx := context.Background()
	p, err := c.List(ctx, "secrets", "apps", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(p)
	assertNoSecret(t, string(got))
	if it := p.Items[0]; it.Type != "Opaque" || strings.Join(it.Keys, ",") != "password,user" || it.Labels["app"] != "db" {
		t.Fatalf("item = %+v", it)
	}
	text, err := c.Get(ctx, "secrets", "apps", "db")
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, text)
	if !strings.Contains(text, `"name": "db"`) {
		t.Fatalf("get lost the object: %s", text)
	}
	if v, err := c.SecretValue(ctx, "apps", "db", "password"); err != nil || v != "hunter2" {
		t.Fatalf("reveal = %q, %v", v, err)
	}
	if _, err := c.SecretValue(ctx, "apps", "db", "missing"); err == nil {
		t.Fatal("a key the secret lacks was revealed")
	}
}

func assertNoSecret(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, "hunter2") || strings.Contains(s, base64.StdEncoding.EncodeToString([]byte("hunter2"))) || strings.Contains(s, "last-applied") {
		t.Fatalf("a secret value reached the page: %s", s)
	}
}

// A name from the page never reaches a URL unless Kubernetes would accept
// it, and a kind outside the browse table is refused.
func TestNamesAreCheckedBeforeAnyRequest(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) })
	ctx := context.Background()
	for name, err := range map[string]error{
		"pod":            func() error { _, err := c.Get(ctx, "pods", "apps", "a;rm -rf /"); return err }(),
		"traversal":      func() error { _, err := c.Get(ctx, "pods", "apps", "../../secrets/x"); return err }(),
		"namespace":      func() error { _, err := c.List(ctx, "pods", "a;rm -rf /", 0, ""); return err }(),
		"delete":         c.Delete(ctx, "pods", "apps", "a b"),
		"scale":          c.Scale(ctx, "deployments", "apps", "UPPER", 1),
		"restart":        c.Restart(ctx, "deployments", "Apps", "web"),
		"secret":         func() error { _, err := c.SecretValue(ctx, "apps", "../x", "k"); return err }(),
		"logs container": func() error { _, err := c.Logs(ctx, "apps", "web-1", "a;b", 10); return err }(),
		"kind":           func() error { _, err := c.List(ctx, "clusterroles", "", 0, ""); return err }(),
		"scale a pod":    c.Scale(ctx, "pods", "apps", "web-1", 1),
	} {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if s := f.seen(); len(s) != 0 {
		t.Fatalf("requests reached the server: %v", s)
	}
}

func TestDeleteScaleAndRestartSendTheirPatches(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) })
	ctx := context.Background()
	if err := c.Delete(ctx, "pods", "apps", "web-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Scale(ctx, "deployments", "apps", "web", 3); err != nil {
		t.Fatal(err)
	}
	if err := c.Restart(ctx, "statefulsets", "apps", "db"); err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /api/v1/namespaces/apps/pods/web-1", "PATCH /apis/apps/v1/namespaces/apps/deployments/web/scale", "PATCH /apis/apps/v1/namespaces/apps/statefulsets/db"}
	if strings.Join(f.seen(), "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %v", f.seen())
	}
	if b := f.body[want[1]]; b != `{"spec":{"replicas":3}}` || f.reqs[1].Header.Get("Content-Type") != "application/merge-patch+json" {
		t.Fatalf("scale sent %s as %s", b, f.reqs[1].Header.Get("Content-Type"))
	}
	if b := f.body[want[2]]; !strings.Contains(b, `"kubectl.kubernetes.io/restartedAt"`) || f.reqs[2].Header.Get("Content-Type") != "application/strategic-merge-patch+json" {
		t.Fatalf("restart sent %s", b)
	}
}

func TestLogsFollowTheChosenContainer(t *testing.T) {
	f, c := newFake(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "line 1\nline 2\n") })
	l, err := c.Logs(context.Background(), "apps", "web-1", "web", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	b, _ := io.ReadAll(l)
	q := f.reqs[0].URL.Query()
	if f.reqs[0].URL.Path != "/api/v1/namespaces/apps/pods/web-1/log" || q.Get("follow") != "true" || q.Get("container") != "web" || q.Get("tailLines") != "100" {
		t.Fatalf("asked %s?%s", f.reqs[0].URL.Path, f.reqs[0].URL.RawQuery)
	}
	if string(b) != "line 1\nline 2\n" {
		t.Fatalf("logs = %q", b)
	}
	if _, err := l.Write([]byte("x")); err == nil {
		t.Fatal("logs took input")
	}
}
