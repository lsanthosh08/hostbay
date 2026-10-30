// Package mcp is Hostbay's MCP server (R63, KTD35): Streamable HTTP on
// 127.0.0.1 with bearer tokens, and the `hostbay mcp` stdio relay. Its
// tools reach saved SSH hosts only through internal/agentexec's gate
// (KTD32, KTD36).
package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"hostbay/internal/agentexec"
	"hostbay/internal/store"
)

// DefaultPort is the port the server listens on unless the user sets one.
const DefaultPort = 47821

// Path is where MCP clients send requests.
const Path = "/mcp"

// The relay's check that a listener is this Hostbay (KTD35). The running
// app writes a secret for this launch into the 0600 endpoint file; before
// the relay sends a token over a connection, it sends a fresh nonce there
// and accepts only the HMAC of it under that secret, so a program that took
// the port while Hostbay was down, and never read the file, cannot answer.
const (
	challengePath   = "/hostbay-challenge"
	challengeHeader = "Hostbay-Challenge"
	proofDomain     = "hostbay mcp endpoint v1"
)

// endpoint is the file the running server keeps in the default folder,
// beside the lock, so it stays put when the data folder moves (KTD42).
type endpoint struct {
	PID    int    `json:"pid"`
	Port   int    `json:"port"`
	Secret string `json:"secret"` // base64 of 32 random bytes, new each start
}

func proof(secret []byte, port int, nonce string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(proofDomain + "\x00" + strconv.Itoa(port) + "\x00" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

func endpointPath(home string) string { return filepath.Join(home, store.EndpointFile) }

func readEndpoint(home string) (endpoint, []byte, error) {
	var e endpoint
	b, err := os.ReadFile(endpointPath(home))
	if err != nil {
		return e, nil, err
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, nil, err
	}
	secret, err := base64.StdEncoding.DecodeString(e.Secret)
	if err != nil || len(secret) != 32 || e.Port <= 0 || e.PID <= 0 {
		return e, nil, errors.New("the MCP endpoint file is damaged")
	}
	return e, secret, nil
}

// writeEndpoint replaces the endpoint file in one rename, 0600 from the
// start.
func writeEndpoint(home string, e endpoint) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(home, ".mcp-endpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), endpointPath(home))
}

// ClearEndpoint removes an endpoint file a crashed run left behind. The
// store lock means no other Hostbay is running.
func ClearEndpoint(home string) { _ = os.Remove(endpointPath(home)) }

// Server is the MCP endpoint, off until Start.
type Server struct {
	St     *store.Store
	Jobs   *agentexec.HostJobs
	Tokens *Tokens
	VPNs   VPNs
	Home   string // the default folder, where the endpoint file goes

	mu     sync.Mutex
	srv    *http.Server
	port   int
	secret []byte
}

// Start listens on 127.0.0.1:port and writes the endpoint file once the
// port is bound. A port in use starts nothing and writes no file.
func (s *Server) Start(port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv != nil {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("the MCP server cannot listen on port %d: %w", port, err)
	}
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	if err := writeEndpoint(s.Home, endpoint{PID: os.Getpid(), Port: port, Secret: base64.StdEncoding.EncodeToString(secret)}); err != nil {
		ln.Close()
		return err
	}
	s.srv = &http.Server{Handler: s.handler(port, secret), ReadHeaderTimeout: 10 * time.Second}
	s.port, s.secret = port, secret
	go s.srv.Serve(ln)
	return nil
}

// Stop closes the listener and removes the endpoint file if it is still
// this launch's.
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if s.srv.Shutdown(ctx) != nil {
		s.srv.Close()
	}
	if _, secret, err := readEndpoint(s.Home); err == nil && hmac.Equal(secret, s.secret) {
		ClearEndpoint(s.Home)
	}
	s.srv, s.port, s.secret = nil, 0, nil
}

// Port is the port the server listens on, or 0 when it is off.
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

func (s *Server) handler(port int, secret []byte) http.Handler {
	server := s.tools()
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxBody})
	mux := http.NewServeMux()
	mux.Handle(Path, guard(port, bearer(s.Tokens, keepRequest(h))))
	mux.Handle(challengePath, guard(port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce := r.Header.Get(challengeHeader)
		if len(nonce) != 64 {
			http.Error(w, "bad challenge", http.StatusBadRequest)
			return
		}
		w.Write([]byte(proof(secret, port, nonce)))
	})))
	return mux
}
