package k8s

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"strings"
)

// Usage is one pod's or node's current use: CPU in millicores, memory in
// bytes.
type Usage struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	CPU       int64  `json:"cpu"`
	Memory    int64  `json:"memory"`
}

// Metrics is metrics.k8s.io's view of a namespace's pods and the nodes.
// Installed is false when the cluster has no metrics API, which the tab
// shows as "not installed" rather than an error. Nodes is empty when the
// user may not read node metrics.
type Metrics struct {
	Installed bool    `json:"installed"`
	Pods      []Usage `json:"pods"`
	Nodes     []Usage `json:"nodes"`
}

type usageList struct {
	Items []struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Usage      map[string]string `json:"usage"`
		Containers []struct {
			Usage map[string]string `json:"usage"`
		} `json:"containers"`
	} `json:"items"`
}

// Metrics reads the CPU and memory of ns's pods ("" for every namespace)
// and of the nodes.
func (c *Client) Metrics(ctx context.Context, ns string) (Metrics, error) {
	path := "/apis/metrics.k8s.io/v1beta1/pods"
	if ns != "" {
		if err := checkLabel("namespace", ns); err != nil {
			return Metrics{}, err
		}
		path = "/apis/metrics.k8s.io/v1beta1/namespaces/" + ns + "/pods"
	}
	m := Metrics{Installed: true, Pods: []Usage{}, Nodes: []Usage{}}
	var pods usageList
	if err := c.getJSON(ctx, path, nil, &pods); err != nil {
		if IsStatus(err, http.StatusNotFound) || IsStatus(err, http.StatusServiceUnavailable) {
			return Metrics{Pods: []Usage{}, Nodes: []Usage{}}, nil
		}
		return Metrics{}, err
	}
	for _, it := range pods.Items {
		u := Usage{Name: it.Metadata.Name, Namespace: it.Metadata.Namespace}
		for _, ct := range it.Containers {
			if err := u.add(ct.Usage); err != nil {
				return Metrics{}, err
			}
		}
		m.Pods = append(m.Pods, u)
	}
	var nodes usageList
	if err := c.getJSON(ctx, "/apis/metrics.k8s.io/v1beta1/nodes", nil, &nodes); err != nil {
		if IsStatus(err, http.StatusForbidden) {
			return m, nil
		}
		return Metrics{}, err
	}
	for _, it := range nodes.Items {
		u := Usage{Name: it.Metadata.Name}
		if err := u.add(it.Usage); err != nil {
			return Metrics{}, err
		}
		m.Nodes = append(m.Nodes, u)
	}
	return m, nil
}

func (u *Usage) add(usage map[string]string) error {
	cpu, err := quantity(usage["cpu"], 1000)
	if err != nil {
		return err
	}
	mem, err := quantity(usage["memory"], 1)
	if err != nil {
		return err
	}
	u.CPU += cpu
	u.Memory += mem
	return nil
}

var suffixes = map[string]*big.Rat{
	"n": big.NewRat(1, 1e9), "u": big.NewRat(1, 1e6), "m": big.NewRat(1, 1e3), "": big.NewRat(1, 1),
	"k": big.NewRat(1e3, 1), "M": big.NewRat(1e6, 1), "G": big.NewRat(1e9, 1), "T": big.NewRat(1e12, 1), "P": big.NewRat(1e15, 1), "E": big.NewRat(1e18, 1),
	"Ki": big.NewRat(1<<10, 1), "Mi": big.NewRat(1<<20, 1), "Gi": big.NewRat(1<<30, 1), "Ti": big.NewRat(1<<40, 1), "Pi": big.NewRat(1<<50, 1), "Ei": big.NewRat(1<<60, 1),
}

// quantity is a Kubernetes quantity ("250m", "64Mi", "1.5") times scale,
// rounded down. "" is 0.
func quantity(s string, scale int64) (int64, error) {
	if s == "" {
		return 0, nil
	}
	num := strings.TrimRight(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	mul, ok := suffixes[s[len(num):]]
	r, valid := new(big.Rat).SetString(num)
	if !ok || !valid || num == "" {
		return 0, fmt.Errorf("the cluster reported a quantity Hostbay cannot read: %q", s)
	}
	r.Mul(r, mul).Mul(r, big.NewRat(scale, 1))
	q := new(big.Int).Quo(r.Num(), r.Denom())
	if !q.IsInt64() {
		return 0, fmt.Errorf("the quantity %q is too large", s)
	}
	return q.Int64(), nil
}
