package k8s

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// discovery serves /api/v1 and /apis/apps/v1 the way a cluster does, and
// answers an apply with the object it was sent, as a server echoes it.
func discovery(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1":
		writeJSON(w, map[string]any{"resources": []any{
			map[string]any{"name": "configmaps", "kind": "ConfigMap", "namespaced": true},
			map[string]any{"name": "secrets", "kind": "Secret", "namespaced": true},
			map[string]any{"name": "namespaces", "kind": "Namespace", "namespaced": false},
			map[string]any{"name": "pods/log", "kind": "Pod", "namespaced": true},
		}})
	case "/apis/apps/v1":
		writeJSON(w, map[string]any{"resources": []any{map[string]any{"name": "deployments", "kind": "Deployment", "namespaced": true}}})
	default:
		if r.Method != http.MethodPatch {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/deployments/") {
			w.WriteHeader(http.StatusCreated)
		}
		w.Write([]byte(`{"kind":"Secret","metadata":{"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"stringData\":{\"password\":\"hunter2\"}}"}},"stringData":{"password":"hunter2"}}`))
	}
}

const manifest = `apiVersion: v1
kind: ConfigMap
metadata:
  name: settings
data:
  mode: fast
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: other
spec:
  replicas: 1
---
apiVersion: v1
kind: Namespace
metadata:
  name: team
`

// Apply resolves each document's path through discovery, applies it
// server side as fieldManager hostbay, and with dryRun only asks the
// server to check it (KTD28).
func TestApplyUsesDiscoveryAndServerSideApply(t *testing.T) {
	f, c := newFake(t, discovery)
	ctx := context.Background()
	res, err := c.Apply(ctx, "apps", manifest, true)
	if err != nil {
		t.Fatal(err)
	}
	var patches []*http.Request
	for _, r := range f.reqs {
		if r.Method == http.MethodPatch {
			patches = append(patches, r)
		}
	}
	paths := []string{"/api/v1/namespaces/apps/configmaps/settings", "/apis/apps/v1/namespaces/other/deployments/web", "/api/v1/namespaces/team"}
	if len(patches) != 3 {
		t.Fatalf("requests = %v", f.seen())
	}
	for i, r := range patches {
		q := r.URL.Query()
		if r.URL.Path != paths[i] || q.Get("fieldManager") != "hostbay" || q.Get("dryRun") != "All" || r.Header.Get("Content-Type") != "application/apply-patch+yaml" {
			t.Fatalf("apply %d: %s?%s as %s", i, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"))
		}
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(f.body["PATCH "+paths[0]]), &sent); err != nil || sent["data"].(map[string]any)["mode"] != "fast" {
		t.Fatalf("sent %s: %v", f.body["PATCH "+paths[0]], err)
	}
	if res[0].Outcome != "configured" || res[1].Outcome != "created" || res[1].Namespace != "other" || res[2].Namespace != "" {
		t.Fatalf("results = %+v", res)
	}
	if _, err := c.Apply(ctx, "apps", manifest, false); err != nil {
		t.Fatal(err)
	}
	if q := f.reqs[len(f.reqs)-1].URL.Query(); q.Has("dryRun") || q.Get("fieldManager") != "hostbay" {
		t.Fatalf("the real apply sent %s", f.reqs[len(f.reqs)-1].URL.RawQuery)
	}
}

// With no namespace chosen (the tab's "All namespaces"), a document without
// its own goes to "default", as kubectl applies it.
func TestApplyWithoutANamespaceUsesDefault(t *testing.T) {
	f, c := newFake(t, discovery)
	if _, err := c.Apply(context.Background(), "", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n", true); err != nil {
		t.Fatal(err)
	}
	if s := f.seen(); s[len(s)-1] != "PATCH /api/v1/namespaces/default/configmaps/settings" {
		t.Fatalf("requests = %v", s)
	}
}

// A Secret applied sends no value and no last-applied copy back to the
// page, even when the server echoes them.
func TestApplyingASecretReturnsNoValue(t *testing.T) {
	_, c := newFake(t, discovery)
	res, err := c.Apply(context.Background(), "apps", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\nstringData:\n  password: hunter2\n", false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	assertNoSecret(t, string(b))
}

func TestApplyRefusesOversizedAliasedAndUnknownDocuments(t *testing.T) {
	f, c := newFake(t, discovery)
	ctx := context.Background()
	big := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: big\ndata:\n  x: \"" + strings.Repeat("a", 2<<20) + "\"\n"
	if _, err := c.Apply(ctx, "apps", big, true); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("a 2 MiB document: %v", err)
	}
	bomb := "a: &a [\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\",\"x\"]\nb: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]\nc: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]\nd: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c]\ne: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d]\nf: &f [*e,*e,*e,*e,*e,*e,*e,*e,*e]\ng: &g [*f,*f,*f,*f,*f,*f,*f,*f,*f]\n"
	if _, err := c.Apply(ctx, "apps", bomb, true); err == nil || !strings.Contains(err.Error(), "alias") {
		t.Fatalf("an alias bomb: %v", err)
	}
	for _, doc := range []string{
		"apiVersion: v1\nkind: Widget\nmetadata:\n  name: w\n",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: Bad_Name\n",
		"apiVersion: ../../x/v1\nkind: ConfigMap\nmetadata:\n  name: a\n",
		"kind: ConfigMap\nmetadata:\n  name: a\n",
	} {
		if _, err := c.Apply(ctx, "apps", doc, true); err == nil {
			t.Errorf("applied %q", doc)
		}
	}
	for _, r := range f.reqs {
		if r.Method == http.MethodPatch {
			t.Fatalf("a refused document reached the server: %s", r.URL.Path)
		}
	}
}
