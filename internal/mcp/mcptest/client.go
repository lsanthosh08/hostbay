// Package mcptest is a scripted MCP client for Hostbay's server, over HTTP
// or over the stdio relay, on a chosen protocol version.
package mcptest

import (
	"context"
	"io"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The protocol versions Hostbay's stateless server is tested on.
const (
	V20251125 = "2025-11-25"
	V20260728 = "2026-07-28"
)

func client() *sdk.Client {
	return sdk.NewClient(&sdk.Implementation{Name: "hostbay-mcptest", Version: "1"}, nil)
}

// HTTP opens a session on url with token ("" sends none).
func HTTP(ctx context.Context, url, token, version string) (*sdk.ClientSession, error) {
	hc := &http.Client{Transport: bearer{token, http.DefaultTransport}}
	t := &sdk.StreamableClientTransport{Endpoint: url, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1}
	return client().Connect(ctx, t, &sdk.ClientSessionOptions{ProtocolVersion: version})
}

// IO opens a session over a relay's stdin (w) and stdout (r).
func IO(ctx context.Context, r io.ReadCloser, w io.WriteCloser, version string) (*sdk.ClientSession, error) {
	return client().Connect(ctx, &sdk.IOTransport{Reader: r, Writer: w}, &sdk.ClientSessionOptions{ProtocolVersion: version})
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.token != "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(r)
}
