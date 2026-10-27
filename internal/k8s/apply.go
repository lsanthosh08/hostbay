package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// MaxApply caps the YAML one apply takes.
const MaxApply = 1 << 20

// Applied is one document's outcome: "created" or "configured" (with
// dryRun, what the apply would do). It names the object only; nothing the
// server sends back reaches the page, so a Secret's values never do.
type Applied struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Outcome   string `json:"outcome"`
}

type doc struct {
	kind     Kind
	ns, name string
	body     []byte
	applied  Applied
}

// Apply applies every document in text server side, as field manager
// hostbay, into ns ("" is "default", as kubectl applies) unless a document
// names its own namespace. With dryRun the server only checks them
// (dryRun=All). Every document is parsed and resolved through discovery
// before the first is sent, so a bad one applies none.
func (c *Client) Apply(ctx context.Context, ns, text string, dryRun bool) ([]Applied, error) {
	if len(text) > MaxApply {
		return nil, errors.New("the YAML is over 1 MiB")
	}
	if ns == "" {
		ns = "default"
	}
	if err := checkLabel("namespace", ns); err != nil {
		return nil, err
	}
	docs, err := c.parse(ctx, ns, text)
	if err != nil {
		return nil, err
	}
	q := url.Values{"fieldManager": {"hostbay"}}
	if dryRun {
		q.Set("dryRun", "All")
	}
	out := []Applied{}
	for _, d := range docs {
		// JSON is YAML, so the apply-patch content type takes it as is.
		resp, err := c.request(ctx, http.MethodPatch, d.kind.path(d.ns, d.name), q, bytes.NewReader(d.body), "application/apply-patch+yaml")
		if err != nil {
			return out, fmt.Errorf("%s %s: %w", d.applied.Kind, d.name, err)
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		d.applied.Outcome = "configured"
		if resp.StatusCode == http.StatusCreated {
			d.applied.Outcome = "created"
		}
		out = append(out, d.applied)
	}
	return out, nil
}

// parse decodes and checks every document.
func (c *Client) parse(ctx context.Context, ns, text string) ([]doc, error) {
	dec := yaml.NewDecoder(bytes.NewReader([]byte(text)))
	var docs []doc
	for i := 1; ; i++ {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		v, err := plain(&n)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if v == nil {
			continue // an empty document, as between two "---"
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("document %d is not an object", i)
		}
		apiVersion, _ := m["apiVersion"].(string)
		kind, _ := m["kind"].(string)
		md, _ := m["metadata"].(map[string]any)
		name, _ := md["name"].(string)
		if apiVersion == "" || kind == "" || name == "" {
			return nil, fmt.Errorf("document %d needs apiVersion, kind and metadata.name", i)
		}
		k, err := c.discover(ctx, apiVersion, kind)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if err := checkName(kind, name); err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		dns := ""
		if k.namespaced {
			dns, _ = md["namespace"].(string)
			if dns == "" {
				dns = ns
			}
			if err := checkLabel("namespace", dns); err != nil {
				return nil, fmt.Errorf("document %d: %w", i, err)
			}
		}
		body, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		docs = append(docs, doc{kind: k, ns: dns, name: name, body: body, applied: Applied{Kind: kind, Name: name, Namespace: dns}})
	}
	if len(docs) == 0 {
		return nil, errors.New("the YAML holds no document")
	}
	return docs, nil
}

// plain turns a YAML node into JSON-ready values. Aliases are refused, so
// a document that expands to far more than it is (an alias bomb) never
// grows; manifests have no need of them.
func plain(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return plain(n.Content[0])
	case yaml.AliasNode:
		return nil, errors.New("YAML aliases are not accepted")
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: a key must be a plain value", k.Line)
			}
			if k.Tag == "!!merge" {
				return nil, errors.New("YAML aliases are not accepted")
			}
			v, err := plain(n.Content[i+1])
			if err != nil {
				return nil, err
			}
			m[k.Value] = v
		}
		return m, nil
	case yaml.SequenceNode:
		s := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := plain(c)
			if err != nil {
				return nil, err
			}
			s = append(s, v)
		}
		return s, nil
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!null":
			return nil, nil
		case "!!bool", "!!int", "!!float":
			var v any
			if err := n.Decode(&v); err != nil {
				return nil, err
			}
			// JSON cannot hold YAML's .inf and .nan.
			if f, ok := v.(float64); ok && (f != f || f > 1e308 || f < -1e308) {
				return nil, fmt.Errorf("line %d: %s is not a number JSON can hold", n.Line, n.Value)
			}
			if i, ok := v.(int); ok {
				return json.Number(strconv.Itoa(i)), nil
			}
			return v, nil
		}
		return n.Value, nil
	}
	return nil, fmt.Errorf("line %d: unsupported YAML", n.Line)
}
