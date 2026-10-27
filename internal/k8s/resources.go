package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Kind is where one kind of object lives in the API.
type Kind struct {
	group, version, resource string
	namespaced               bool
}

// path is the URL path of the kind's collection in ns ("" for every
// namespace), or of the object name in it. Every part is already checked.
func (k Kind) path(ns, name string) string {
	p := "/api/" + k.version
	if k.group != "" {
		p = "/apis/" + k.group + "/" + k.version
	}
	if k.namespaced && ns != "" {
		p += "/namespaces/" + ns
	}
	p += "/" + k.resource
	if name != "" {
		p += "/" + name
	}
	return p
}

// kinds are what the tab browses, by the name the page sends.
var kinds = map[string]Kind{
	"namespaces":   {"", "v1", "namespaces", false},
	"nodes":        {"", "v1", "nodes", false},
	"pods":         {"", "v1", "pods", true},
	"services":     {"", "v1", "services", true},
	"configmaps":   {"", "v1", "configmaps", true},
	"secrets":      {"", "v1", "secrets", true},
	"events":       {"", "v1", "events", true},
	"deployments":  {"apps", "v1", "deployments", true},
	"statefulsets": {"apps", "v1", "statefulsets", true},
	"daemonsets":   {"apps", "v1", "daemonsets", true},
	"jobs":         {"batch", "v1", "jobs", true},
	"ingresses":    {"networking.k8s.io", "v1", "ingresses", true},
}

// target checks the page's kind, namespace and object name, and returns
// the kind's path parts. name "" is the collection.
func target(kind, ns, name string) (Kind, error) {
	k, ok := kinds[kind]
	if !ok {
		return Kind{}, fmt.Errorf("Hostbay does not browse %q", kind)
	}
	if !k.namespaced {
		ns = ""
	}
	if ns != "" {
		if err := checkLabel("namespace", ns); err != nil {
			return Kind{}, err
		}
	}
	if name != "" {
		if err := checkName(strings.TrimSuffix(k.resource, "s"), name); err != nil {
			return Kind{}, err
		}
	}
	return k, nil
}

// Item is one object as the list shows it. Secrets carry their key names
// only, never a value.
type Item struct {
	Name       string            `json:"name"`
	Namespace  string            `json:"namespace,omitempty"`
	Created    string            `json:"created,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Status     string            `json:"status,omitempty"`
	Ready      string            `json:"ready,omitempty"`    // "ready/wanted": pods' containers, workloads' replicas
	Restarts   int               `json:"restarts,omitempty"` // pods
	Containers []string          `json:"containers,omitempty"`
	Keys       []string          `json:"keys,omitempty"` // config maps and secrets
	Type       string            `json:"type,omitempty"` // secrets and services
	Detail     string            `json:"detail,omitempty"`
}

// Page is one page of a list; Continue fetches the next ("" at the end).
type Page struct {
	Items    []Item `json:"items"`
	Continue string `json:"continue"`
}

// object holds the fields Items are made from. A Secret's data decodes to
// its key names only, so no value is ever held for the page.
type object struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		CreationTimestamp string            `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
	} `json:"metadata"`
	Type string                     `json:"type"`
	Data map[string]json.RawMessage `json:"data"`
	Spec struct {
		Replicas    *int   `json:"replicas"`
		Completions *int   `json:"completions"`
		Type        string `json:"type"`
		ClusterIP   string `json:"clusterIP"`
		Containers  []struct {
			Name string `json:"name"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase                  string `json:"phase"`
		ReadyReplicas          int    `json:"readyReplicas"`
		NumberReady            int    `json:"numberReady"`
		DesiredNumberScheduled int    `json:"desiredNumberScheduled"`
		Succeeded              int    `json:"succeeded"`
		Conditions             []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			Ready        bool `json:"ready"`
			RestartCount int  `json:"restartCount"`
		} `json:"containerStatuses"`
	} `json:"status"`
	// Events
	Reason         string `json:"reason"`
	Message        string `json:"message"`
	InvolvedObject struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"involvedObject"`
}

func (o *object) item(kind string) Item {
	it := Item{Name: o.Metadata.Name, Namespace: o.Metadata.Namespace, Created: o.Metadata.CreationTimestamp, Labels: o.Metadata.Labels}
	s := &o.Status
	wanted := 1
	if o.Spec.Replicas != nil {
		wanted = *o.Spec.Replicas
	}
	switch kind {
	case "pods":
		it.Status = s.Phase
		ready := 0
		for _, c := range s.ContainerStatuses {
			if c.Ready {
				ready++
			}
			it.Restarts += c.RestartCount
		}
		for _, c := range o.Spec.Containers {
			it.Containers = append(it.Containers, c.Name)
		}
		it.Ready = fmt.Sprintf("%d/%d", ready, len(o.Spec.Containers))
	case "deployments", "statefulsets":
		it.Ready = fmt.Sprintf("%d/%d", s.ReadyReplicas, wanted)
	case "daemonsets":
		it.Ready = fmt.Sprintf("%d/%d", s.NumberReady, s.DesiredNumberScheduled)
	case "jobs":
		if o.Spec.Completions != nil {
			wanted = *o.Spec.Completions
		}
		it.Ready = fmt.Sprintf("%d/%d", s.Succeeded, wanted)
	case "services":
		it.Type, it.Status = o.Spec.Type, o.Spec.ClusterIP
	case "namespaces":
		it.Status = s.Phase
	case "nodes":
		it.Status = "NotReady"
		for _, c := range s.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				it.Status = "Ready"
			}
		}
	case "events":
		it.Status, it.Detail = o.Reason, o.InvolvedObject.Kind+"/"+o.InvolvedObject.Name+": "+o.Message
	case "secrets", "configmaps":
		it.Type = o.Type
		for k := range o.Data {
			it.Keys = append(it.Keys, k)
		}
		slices.Sort(it.Keys)
	}
	return it
}

// List returns up to limit objects of kind in ns ("" for every namespace)
// from cont, the previous page's Continue.
func (c *Client) List(ctx context.Context, kind, ns string, limit int, cont string) (Page, error) {
	k, err := target(kind, ns, "")
	if err != nil {
		return Page{}, err
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if cont != "" {
		q.Set("continue", cont)
	}
	var list struct {
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
		Items []object `json:"items"`
	}
	if err := c.getJSON(ctx, k.path(ns, ""), q, &list); err != nil {
		return Page{}, err
	}
	p := Page{Items: []Item{}, Continue: list.Metadata.Continue}
	for i := range list.Items {
		p.Items = append(p.Items, list.Items[i].item(kind))
	}
	return p, nil
}

// lastApplied holds the object as kubectl last applied it, values and all.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// Get returns one object as indented JSON, without its managed-fields
// bookkeeping. A Secret comes without its data and last-applied copy.
func (c *Client) Get(ctx context.Context, kind, ns, name string) (string, error) {
	k, err := target(kind, ns, name)
	if err != nil {
		return "", err
	}
	var obj map[string]any
	if err := c.getJSON(ctx, k.path(ns, name), nil, &obj); err != nil {
		return "", err
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
	}
	stripSecret(obj)
	b, err := json.MarshalIndent(obj, "", "  ")
	return string(b), err
}

// stripSecret removes a Secret object's values (KTD28).
func stripSecret(obj map[string]any) {
	if obj["kind"] != "Secret" {
		return
	}
	delete(obj, "data")
	delete(obj, "stringData")
	if md, ok := obj["metadata"].(map[string]any); ok {
		if a, ok := md["annotations"].(map[string]any); ok {
			delete(a, lastApplied)
		}
	}
}

// SecretValue reveals one key of a Secret.
func (c *Client) SecretValue(ctx context.Context, ns, name, key string) (string, error) {
	k, err := target("secrets", ns, name)
	if err != nil {
		return "", err
	}
	var s struct {
		Data map[string]string `json:"data"`
	}
	if err := c.getJSON(ctx, k.path(ns, name), nil, &s); err != nil {
		return "", err
	}
	v, ok := s.Data[key]
	if !ok {
		return "", fmt.Errorf("the secret %s has no key %q", name, key)
	}
	b, err := base64.StdEncoding.DecodeString(v)
	return string(b), err
}

// Delete deletes one object; its dependents go in the background.
func (c *Client) Delete(ctx context.Context, kind, ns, name string) error {
	k, err := target(kind, ns, name)
	if err != nil {
		return err
	}
	return c.send(ctx, http.MethodDelete, k.path(ns, name), url.Values{"propagationPolicy": {"Background"}}, nil, "")
}

// Scale sets a deployment's or stateful set's replicas.
func (c *Client) Scale(ctx context.Context, kind, ns, name string, replicas int) error {
	if kind != "deployments" && kind != "statefulsets" {
		return errors.New("only deployments and stateful sets scale")
	}
	if replicas < 0 || replicas > 10000 {
		return errors.New("replicas must be between 0 and 10000")
	}
	k, err := target(kind, ns, name)
	if err != nil {
		return err
	}
	body := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	return c.send(ctx, http.MethodPatch, k.path(ns, name)+"/scale", nil, strings.NewReader(body), "application/merge-patch+json")
}

// Restart rolls a deployment, stateful set or daemon set's pods, as
// kubectl rollout restart does: by stamping its pod template.
func (c *Client) Restart(ctx context.Context, kind, ns, name string) error {
	if kind != "deployments" && kind != "statefulsets" && kind != "daemonsets" {
		return errors.New("only deployments, stateful sets and daemon sets restart")
	}
	k, err := target(kind, ns, name)
	if err != nil {
		return err
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{"kubectl.kubernetes.io/restartedAt": time.Now().UTC().Format(time.RFC3339)}}}}})
	return c.send(ctx, http.MethodPatch, k.path(ns, name), nil, strings.NewReader(string(patch)), "application/strategic-merge-patch+json")
}

// send runs a request whose answer is not needed.
func (c *Client) send(ctx context.Context, method, path string, q url.Values, body io.Reader, ctype string) error {
	resp, err := c.request(ctx, method, path, q, body, ctype)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	return resp.Body.Close()
}
