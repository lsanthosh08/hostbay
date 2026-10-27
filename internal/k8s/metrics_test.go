package k8s

import (
	"context"
	"net/http"
	"testing"
)

func TestMetricsSumContainersAndParseQuantities(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/apis/metrics.k8s.io/v1beta1/namespaces/apps/pods":
			writeJSON(w, map[string]any{"items": []any{map[string]any{
				"metadata": map[string]any{"name": "web-1", "namespace": "apps"},
				"containers": []any{
					map[string]any{"usage": map[string]string{"cpu": "250000000n", "memory": "64Mi"}},
					map[string]any{"usage": map[string]string{"cpu": "5m", "memory": "1024Ki"}},
				},
			}}})
		case "/apis/metrics.k8s.io/v1beta1/nodes":
			writeJSON(w, map[string]any{"items": []any{map[string]any{
				"metadata": map[string]any{"name": "node-1"},
				"usage":    map[string]string{"cpu": "2", "memory": "1Gi"},
			}}})
		default:
			http.NotFound(w, r)
		}
	})
	m, err := c.Metrics(context.Background(), "apps")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Installed || len(m.Pods) != 1 || len(m.Nodes) != 1 {
		t.Fatalf("metrics = %+v", m)
	}
	if p := m.Pods[0]; p.Name != "web-1" || p.CPU != 255 || p.Memory != 65<<20 {
		t.Fatalf("pod = %+v", p)
	}
	if n := m.Nodes[0]; n.CPU != 2000 || n.Memory != 1<<30 {
		t.Fatalf("node = %+v", n)
	}
}

// Without metrics-server the API is absent: "not installed", not an error.
func TestMetricsAbsentIsNotInstalled(t *testing.T) {
	_, c := newFake(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	m, err := c.Metrics(context.Background(), "apps")
	if err != nil || m.Installed {
		t.Fatalf("metrics = %+v, %v", m, err)
	}
}

func TestQuantities(t *testing.T) {
	for in, want := range map[string]int64{"1": 1, "1500m": 1, "2k": 2000, "1M": 1000000, "3Ki": 3072, "1.5Gi": 1610612736, "100e3": 100000} {
		if got, err := quantity(in, 1); err != nil || got != want {
			t.Errorf("quantity(%s) = %d, %v; want %d", in, got, err, want)
		}
	}
	if got, _ := quantity("1500m", 1000); got != 1500 {
		t.Errorf("1500m in millis = %d", got)
	}
	if _, err := quantity("lots", 1); err == nil {
		t.Error("a malformed quantity was accepted")
	}
}
