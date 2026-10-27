// Package k8s is Hostbay's small Kubernetes API client (R57, KTD28): paged
// lists of a fixed set of resource kinds, delete, scale and restart,
// server-side apply after a dry run, pod logs, pod exec over the WebSocket
// channel protocol, and metrics.k8s.io. It runs on transfer.HTTPClient, so
// every connection goes through the host's dial function and the API
// server is never left for another origin, and it reads no proxy, token or
// kubeconfig from the environment.
//
// Names, namespaces and API groups from the page are checked against the
// DNS-1123 rules Kubernetes itself applies before any of them reach a URL,
// and resource paths come from a fixed table or from the cluster's own
// discovery, never from page text.
package k8s

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"hostbay/internal/transfer"
)

// Config is how a client reaches one cluster. URL is the API server
// (https only), TLS its verified config with any client certificate, and
// Dial the host's one dial function. Token is the bearer token, or "" for
// a client certificate alone.
type Config struct {
	URL   string
	TLS   *tls.Config
	Dial  transfer.DialFunc
	Token string
}

// Client talks to one cluster. It is safe for concurrent use.
type Client struct {
	base  *url.URL
	hc    *http.Client
	token string

	mu    sync.Mutex
	kinds map[string]Kind // apiVersion + "\x00" + kind -> discovered resource
}

// maxBody caps a JSON response read into memory; lists are paged far below it.
const maxBody = 32 << 20

// New makes a client for cfg. It opens no connection.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("the cluster's server must be an https:// URL")
	}
	if cfg.TLS == nil {
		return nil, errors.New("the cluster has no TLS configuration")
	}
	return &Client{base: u, hc: transfer.HTTPClient(cfg.TLS, u, cfg.Dial), token: cfg.Token, kinds: map[string]Kind{}}, nil
}

// Close closes the client's idle connections; streams already open (logs,
// exec) are closed by their own Close.
func (c *Client) Close() { c.hc.CloseIdleConnections() }

// StatusError is a request the API server refused, with its own Status
// message.
type StatusError struct {
	Code    int
	Reason  string
	Message string
}

func (e *StatusError) Error() string { return e.Message }

// IsStatus reports err as a refusal with HTTP status code.
func IsStatus(err error, code int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}

var (
	labelRE     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	subdomainRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	kindRE      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,62}$`)
)

// checkLabel refuses s unless it is a DNS-1123 label, as namespaces and
// container names are.
func checkLabel(what, s string) error {
	if len(s) > 63 || !labelRE.MatchString(s) {
		return fmt.Errorf("%q is not a valid %s name", s, what)
	}
	return nil
}

// checkName refuses s unless it is a DNS-1123 subdomain, as the names of
// the objects Hostbay browses are.
func checkName(what, s string) error {
	if len(s) > 253 || !subdomainRE.MatchString(s) {
		return fmt.Errorf("%q is not a valid %s name", s, what)
	}
	return nil
}

// endpoint is the request URL for path (already built from checked parts)
// under the server's own path prefix.
func (c *Client) endpoint(path string, q url.Values) *url.URL {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawPath = ""
	u.RawQuery = q.Encode()
	return &u
}

// request sends one request and returns its response, or the server's
// refusal as a StatusError. The caller closes the body.
func (c *Client) request(ctx context.Context, method, path string, q url.Values, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, q).String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, statusError(resp)
	}
	return resp, nil
}

// statusError reads the Status object a refusal carries.
func statusError(resp *http.Response) error {
	var st struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if json.Unmarshal(b, &st) != nil || st.Message == "" {
		st.Message = "the cluster answered " + resp.Status
	}
	return &StatusError{Code: resp.StatusCode, Reason: st.Reason, Message: st.Message}
}

// getJSON decodes a GET of path into out.
func (c *Client) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	resp, err := c.request(ctx, http.MethodGet, path, q, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out)
}

// Version is the server's version. Hostbay's connect probe: it runs the
// TLS check and, with a token, its sign-in.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	err := c.getJSON(ctx, "/version", nil, &v)
	return v.GitVersion, err
}

// discover finds the resource serving kind in apiVersion from the
// cluster's discovery documents (/api/<v> or /apis/<group>/<v>), so an
// applied document's path is the cluster's own, never built from its text.
func (c *Client) discover(ctx context.Context, apiVersion, kind string) (Kind, error) {
	group, version := "", apiVersion
	if i := strings.LastIndex(apiVersion, "/"); i >= 0 {
		group, version = apiVersion[:i], apiVersion[i+1:]
		if err := checkName("API group", group); err != nil {
			return Kind{}, err
		}
	}
	if err := checkLabel("API version", version); err != nil {
		return Kind{}, err
	}
	if !kindRE.MatchString(kind) {
		return Kind{}, fmt.Errorf("%q is not a valid kind", kind)
	}
	key := apiVersion + "\x00" + kind
	c.mu.Lock()
	k, ok := c.kinds[key]
	c.mu.Unlock()
	if ok {
		return k, nil
	}
	path := "/api/" + version
	if group != "" {
		path = "/apis/" + group + "/" + version
	}
	var list struct {
		Resources []struct {
			Name       string `json:"name"`
			Kind       string `json:"kind"`
			Namespaced bool   `json:"namespaced"`
		} `json:"resources"`
	}
	if err := c.getJSON(ctx, path, nil, &list); err != nil {
		if IsStatus(err, http.StatusNotFound) {
			return Kind{}, fmt.Errorf("the cluster does not serve %s", apiVersion)
		}
		return Kind{}, err
	}
	for _, r := range list.Resources {
		// "pods/log" and the like are subresources, not kinds.
		if r.Kind == kind && !strings.Contains(r.Name, "/") && labelRE.MatchString(r.Name) {
			k = Kind{group: group, version: version, resource: r.Name, namespaced: r.Namespaced}
			c.mu.Lock()
			c.kinds[key] = k
			c.mu.Unlock()
			return k, nil
		}
	}
	return Kind{}, fmt.Errorf("the cluster serves no %s in %s", kind, apiVersion)
}
