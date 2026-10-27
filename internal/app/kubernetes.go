package app

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"

	"hostbay/internal/apperr"
	"hostbay/internal/database"
	"hostbay/internal/k8s"
	"hostbay/internal/store"
	"hostbay/internal/tlstrust"
	"hostbay/internal/transfer"
)

// KubernetesService opens Kubernetes hosts in Go and serves their tabs
// (R57, KTD28). Connect returns a cluster ID minted here; unknown IDs get
// nothing. The token and client key stay in Go, and every error is
// scrubbed of the tokens before it leaves. Each tab keeps the cluster as
// it was when it connected.
type KubernetesService struct {
	creds *HostCredentials
	conns *ConnService
	certs *tlstrust.Store

	mu       sync.Mutex
	clusters map[string]*kubeCluster
}

type kubeCluster struct {
	client *k8s.Client
	token  string
}

// ClusterInfo is an open cluster: its ID, the context it was imported
// from, the namespace its tab opens on, and the server's address and
// version for the toolbar.
type ClusterInfo struct {
	ClusterID string `json:"clusterId"`
	Context   string `json:"context"`
	Namespace string `json:"namespace"`
	Server    string `json:"server"`
	Version   string `json:"version"`
}

func NewKubernetesService(creds *HostCredentials, conns *ConnService, certs *tlstrust.Store) *KubernetesService {
	return &KubernetesService{creds: creds, conns: conns, certs: certs, clusters: map[string]*kubeCluster{}}
}

// KubernetesConnect opens a Kubernetes host and returns its cluster ID.
// The API server is reached only through the host's jump host or proxy,
// never around them (KTD24). With CA data that CA is the only trust
// anchor; without, an unknown self-signed certificate fails with
// HostKeyUnknown (details.kind "kubernetesCert"), answered with
// KubernetesTrustCert, and a changed one with HostKeyChanged and a
// removeId for KubernetesRemovePin. Either way no token was sent. A
// refused token is SecretNeeded.
func (s *KubernetesService) KubernetesConnect(ctx context.Context, hostID string) (ClusterInfo, error) {
	c, err := s.creds.Lookup(hostID)
	if err != nil {
		return ClusterInfo{}, err
	}
	h := c.Host
	if h.KindName() != "kubernetes" {
		return ClusterInfo{}, errors.New("this host is not a Kubernetes cluster")
	}
	cl, version, err := s.open(ctx, h, c.Password, c.Key)
	if err != nil {
		return ClusterInfo{}, withHostID(s.openErr(err, h, c.Password), hostID)
	}
	id := newRequestID()
	s.mu.Lock()
	s.clusters[id] = &kubeCluster{client: cl, token: c.Password}
	s.mu.Unlock()
	s.creds.touch(hostID)
	return ClusterInfo{ClusterID: id, Context: h.Alias, Namespace: store.Deref(h.Namespace), Server: store.Deref(h.URL), Version: version}, nil
}

// open connects to h's API server and checks it answers, signing in with
// token or the key-store certificate and key.
func (s *KubernetesService) open(ctx context.Context, h store.Host, token string, key []byte) (*k8s.Client, string, error) {
	raw := store.Deref(h.URL)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, "", errors.New("this cluster's server is not an https:// URL")
	}
	// Dial exactly what the HTTP transport dials for the URL, so the
	// allowlist and the token's binding name the same address.
	h.Hostname, h.Port = clusterAddr(raw)
	dial := allowlisted(h, h.Port, func(ctx context.Context) (net.Conn, error) { return s.conns.dialHost(ctx, &h, h.Port) })
	var certs []tls.Certificate
	if len(key) > 0 {
		pair, err := tls.X509KeyPair(key, key)
		if err != nil {
			return nil, "", errors.New("the saved client certificate does not open; import the kubeconfig again")
		}
		certs = []tls.Certificate{pair}
	}
	serverName := store.Deref(h.TLSServerName)
	if serverName == "" {
		serverName = h.Hostname
	}
	var cl *k8s.Client
	var version string
	try := func(cfg *tls.Config) error {
		cfg = cfg.Clone()
		cfg.Certificates = certs
		c, err := k8s.New(k8s.Config{URL: raw, TLS: cfg, Dial: transfer.DialFunc(dial), Token: token})
		if err != nil {
			return err
		}
		if version, err = c.Version(ctx); err != nil {
			c.Close() // its pooled connection would hold the jump lease
			return err
		}
		cl = c
		return nil
	}
	if h.CAData != nil && *h.CAData != "" {
		cfg, err := tlstrust.Anchored([]byte(*h.CAData), serverName)
		if err != nil {
			return nil, "", err
		}
		err = try(cfg)
		return cl, version, err
	}
	// Pinned by host ID and the name checked, whatever the jump host or proxy.
	if _, err = s.certs.Connect("kubernetes", h.ID, serverName, h.Port, try); err != nil && cl != nil {
		cl.Close()
		cl = nil
	}
	return cl, version, err
}

// openErr is what the page sees of a failed connect: a changed
// certificate gets the ID that removes its pin, a refused sign-in asks for
// the token, and every error loses the token.
func (s *KubernetesService) openErr(err error, h store.Host, token string) error {
	var ae *apperr.Error
	switch {
	case errors.As(err, &ae) && ae.Code == apperr.HostKeyChanged && ae.Details["kind"] == "kubernetesCert":
		ae.With("removeId", s.certs.RemovalRequest("kubernetes", h.ID, ae.Details["host"], h.Port))
	case k8s.IsStatus(err, http.StatusUnauthorized):
		return apperr.New(apperr.SecretNeeded, "The cluster refused the saved token or client certificate; enter a token, or import the kubeconfig again.").With("reason", "wrong")
	}
	return scrubErr(err, token)
}

// scrubErr is err without any of secrets in its message.
func scrubErr(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		ae.Message = database.Scrub(ae.Message, secrets...)
		return ae
	}
	return errors.New(database.Scrub(err.Error(), secrets...))
}

// cluster returns the open cluster clusterID.
func (s *KubernetesService) cluster(clusterID string) (*kubeCluster, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.clusters[clusterID]; c != nil {
		return c, nil
	}
	return nil, errors.New("this cluster connection has closed; connect again")
}

// pageErr is err as the page may see it: without any open cluster's token.
func (s *KubernetesService) pageErr(err error) error {
	s.mu.Lock()
	tokens := make([]string, 0, len(s.clusters))
	for _, c := range s.clusters {
		tokens = append(tokens, c.token)
	}
	s.mu.Unlock()
	return scrubErr(err, tokens...)
}

// run calls fn with the open cluster and scrubs its error.
func run[T any](s *KubernetesService, clusterID string, fn func(*k8s.Client) (T, error)) (T, error) {
	c, err := s.cluster(clusterID)
	if err != nil {
		var zero T
		return zero, err
	}
	v, err := fn(c.client)
	return v, s.pageErr(err)
}

// KubernetesList lists one page of kind ("pods", "deployments", ...) in
// namespace ("" for every namespace) from cont, the previous page's
// continue token. Secrets come as names, types, labels and key names only.
func (s *KubernetesService) KubernetesList(ctx context.Context, clusterID, kind, namespace, cont string) (k8s.Page, error) {
	return run(s, clusterID, func(c *k8s.Client) (k8s.Page, error) { return c.List(ctx, kind, namespace, 0, cont) })
}

// KubernetesGet returns one object as JSON; a Secret without its values.
func (s *KubernetesService) KubernetesGet(ctx context.Context, clusterID, kind, namespace, name string) (string, error) {
	return run(s, clusterID, func(c *k8s.Client) (string, error) { return c.Get(ctx, kind, namespace, name) })
}

// KubernetesRevealSecret returns one key's value of a Secret.
func (s *KubernetesService) KubernetesRevealSecret(ctx context.Context, clusterID, namespace, name, key string) (string, error) {
	return run(s, clusterID, func(c *k8s.Client) (string, error) { return c.SecretValue(ctx, namespace, name, key) })
}

// KubernetesDelete deletes one object. The tab confirms first, naming the
// cluster, context and namespace.
func (s *KubernetesService) KubernetesDelete(ctx context.Context, clusterID, kind, namespace, name string) error {
	_, err := run(s, clusterID, func(c *k8s.Client) (struct{}, error) { return struct{}{}, c.Delete(ctx, kind, namespace, name) })
	return err
}

// KubernetesScale sets a deployment's or stateful set's replicas.
func (s *KubernetesService) KubernetesScale(ctx context.Context, clusterID, kind, namespace, name string, replicas int) error {
	_, err := run(s, clusterID, func(c *k8s.Client) (struct{}, error) {
		return struct{}{}, c.Scale(ctx, kind, namespace, name, replicas)
	})
	return err
}

// KubernetesRestart restarts a workload's pods, as kubectl rollout restart.
func (s *KubernetesService) KubernetesRestart(ctx context.Context, clusterID, kind, namespace, name string) error {
	_, err := run(s, clusterID, func(c *k8s.Client) (struct{}, error) { return struct{}{}, c.Restart(ctx, kind, namespace, name) })
	return err
}

// KubernetesApply applies YAML (up to 1 MiB, several documents allowed)
// server side as field manager hostbay, into namespace unless a document
// names its own. With dryRun the server only checks it; the tab runs the
// dry run first and shows its result before the real apply.
func (s *KubernetesService) KubernetesApply(ctx context.Context, clusterID, namespace, yaml string, dryRun bool) ([]k8s.Applied, error) {
	return run(s, clusterID, func(c *k8s.Client) ([]k8s.Applied, error) { return c.Apply(ctx, namespace, yaml, dryRun) })
}

// KubernetesMetrics reads CPU and memory from metrics.k8s.io; Installed
// is false when the cluster has none.
func (s *KubernetesService) KubernetesMetrics(ctx context.Context, clusterID, namespace string) (k8s.Metrics, error) {
	return run(s, clusterID, func(c *k8s.Client) (k8s.Metrics, error) { return c.Metrics(ctx, namespace) })
}

// KubernetesLogs follows a pod container's log and returns its terminal
// ID: the text arrives as pty-data events after PtyAttach, and PtyClose
// or KubernetesClose ends it.
func (s *KubernetesService) KubernetesLogs(ctx context.Context, clusterID, namespace, pod, container string) (string, error) {
	return run(s, clusterID, func(c *k8s.Client) (string, error) {
		l, err := c.Logs(ctx, namespace, pod, container, 1000)
		if err != nil {
			return "", err
		}
		return s.conns.terms.Open(clusterID, l), nil
	})
}

// KubernetesExec opens a terminal running command (an argument list, such
// as the shell the user picked; never run through a shell) in a pod's
// container. It lives in ConnService's terminal registry under the
// cluster's ID, so KubernetesClose or Disconnect(clusterID) closes it.
func (s *KubernetesService) KubernetesExec(ctx context.Context, clusterID, namespace, pod, container string, command []string, cols, rows int) (string, error) {
	return run(s, clusterID, func(c *k8s.Client) (string, error) {
		t, err := c.Exec(ctx, namespace, pod, container, command, cols, rows)
		if err != nil {
			return "", err
		}
		return s.conns.terms.Open(clusterID, t), nil
	})
}

// KubernetesClose closes a cluster tab with its log and exec terminals.
// Unknown IDs are already closed.
func (s *KubernetesService) KubernetesClose(clusterID string) error {
	s.mu.Lock()
	c := s.clusters[clusterID]
	delete(s.clusters, clusterID)
	s.mu.Unlock()
	s.conns.terms.CloseOwner(clusterID)
	if c != nil {
		c.client.Close()
	}
	return nil
}

// KubernetesTrustCert answers the HostKeyUnknown prompt (details.kind
// "kubernetesCert") carrying requestID; accepting pins the certificate.
func (s *KubernetesService) KubernetesTrustCert(requestID string, accept bool) error {
	return s.certs.Answer(requestID, accept)
}

// KubernetesRemovePin removes the pinned certificate a HostKeyChanged
// refusal named by removeId, after the user typed the refused host name
// and passed OS verification (KTD25); the next connect asks to pin again.
func (s *KubernetesService) KubernetesRemovePin(ctx context.Context, removeID, typedHost string) error {
	return s.certs.RemovePin(ctx, removeID, typedHost, s.creds.vault.Gate)
}
