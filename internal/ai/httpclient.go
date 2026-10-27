package ai

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Limits on what a provider may send back (KTD31). There is no retry once
// a stream has started.
const (
	maxErrorBody  = 64 << 10
	maxStream     = 16 << 20
	maxLine       = 1 << 20
	dialTimeout   = 15 * time.Second
	headerTimeout = 2 * time.Minute
	idleTimeout   = 3 * time.Minute // between bytes of a stream
)

// Dial opens a TCP connection to addr (host:port): through the provider's
// saved proxy, or directly when it has none. A failed proxy fails the
// request; nothing dials around it (KTD22).
type Dial func(ctx context.Context, addr string) (net.Conn, error)

// Pinner finds the TLS config for a server whose certificate the system
// roots may not vouch for (tlstrust.Store).
type Pinner interface {
	Connect(kind, hostID, host string, port int, try func(*tls.Config) error) (*tls.Config, error)
}

// Endpoint is where one provider's requests go. Its key is not here: the
// clients get it on their own, so this can cross into errors and tests.
type Endpoint struct {
	ID          string // the provider's ID, which keys its certificate pin
	BaseURL     string
	Unencrypted bool // http:// to a host that is not this computer, chosen explicitly
}

// HTTPClient is the one client a provider's requests use: no proxy from the
// environment, every connection through dial and only to the base URL's
// host and port, TLS always verified (system roots, else a pin the user
// accepted under the "ai" kind), and every redirect refused, because Go
// copies custom headers such as x-api-key to a redirect's target.
func HTTPClient(ctx context.Context, ep Endpoint, dial Dial, pins Pinner) (*http.Client, error) {
	u, err := url.Parse(ep.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("enter the provider's http(s) base URL")
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	addr := net.JoinHostPort(u.Hostname(), port)
	only := func(ctx context.Context, network, a string) (net.Conn, error) {
		if a != addr {
			return nil, fmt.Errorf("refusing to connect to %s: the provider is at %s", a, addr)
		}
		ctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		return dial(ctx, a)
	}
	var cfg *tls.Config
	if u.Scheme == "https" {
		n, _ := strconv.Atoi(port)
		cfg, err = pins.Connect("ai", ep.ID, u.Hostname(), n, func(c *tls.Config) error {
			conn, err := only(ctx, "tcp", addr)
			if err != nil {
				return err
			}
			defer conn.Close()
			hctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			return tls.Client(conn, c).HandshakeContext(hctx)
		})
		if err != nil {
			return nil, err
		}
	} else if !ep.Unencrypted && !IsLoopback(u.Hostname()) {
		return nil, errors.New("an http:// provider must be on this computer; for another host use https, or mark it unencrypted")
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil, // never the environment's
			DialContext:           only,
			TLSClientConfig:       cfg,
			TLSHandshakeTimeout:   dialTimeout,
			ResponseHeaderTimeout: headerTimeout,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(r *http.Request, _ []*http.Request) error {
			return fmt.Errorf("the provider redirected to %s://%s; Hostbay does not follow redirects", r.URL.Scheme, r.URL.Host)
		},
	}, nil
}

// IsLoopback reports whether host names this computer.
func IsLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// StatusError is a provider's refusal: its HTTP status and what its error
// body said, cut short.
type StatusError struct {
	Status  int
	Type    string
	Message string
}

func (e *StatusError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Type != "" {
		return fmt.Sprintf("the provider answered %d (%s): %s", e.Status, e.Type, msg)
	}
	return fmt.Sprintf("the provider answered %d: %s", e.Status, msg)
}

// statusError reads at most maxErrorBody of a failed response.
func statusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	typ, msg := errorFields(body)
	if msg == "" && typ == "" {
		msg = clip(string(body), 300)
	}
	return &StatusError{Status: resp.StatusCode, Type: typ, Message: msg}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// watchdog cancels a request when no byte of its response arrives for
// idleTimeout, and stops it past maxStream.
type watchdog struct {
	r      io.Reader
	t      *time.Timer
	read   int
	cancel func()
}

func newWatchdog(r io.Reader, cancel func()) *watchdog {
	return &watchdog{r: r, cancel: cancel, t: time.AfterFunc(idleTimeout, cancel)}
}

func (w *watchdog) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	w.t.Reset(idleTimeout)
	w.read += n
	if w.read > maxStream {
		w.cancel()
		return n, errors.New("the provider's reply is larger than 16 MiB")
	}
	return n, err
}

func (w *watchdog) stop() { w.t.Stop() }
