package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// ErrNotRunning is the relay's answer when no running Hostbay serves MCP.
var ErrNotRunning = errors.New("Hostbay is not running with its MCP server on: open Hostbay and turn the server on in Settings > MCP")

// errNotHostbay is a listener on the port that could not prove it is the
// running Hostbay; the token is never sent to it.
var errNotHostbay = errors.New("the program on Hostbay's MCP port is not this Hostbay; nothing was sent to it")

const maxMessage = 4 << 20

// Relay is `hostbay mcp` (KTD35): each JSON-RPC message read from in goes
// to the running Hostbay's endpoint with token, and each reply is written
// to out, one per line. Only MCP messages reach out; diagnostics go to
// logw. It never opens the store or takes the lock, and when Hostbay is
// not running it returns ErrNotRunning without dialling.
func Relay(ctx context.Context, home, token string, in io.Reader, out, logw io.Writer) error {
	if _, err := live(home); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // the client is gone: its open requests end, withdrawing their cards
	r := &relay{home: home, token: token, out: out, log: logw}
	// Every connection proves it reaches this Hostbay before it carries a
	// token, so one opened after Hostbay quit, to whatever took the port,
	// fails instead. No proxy: the endpoint is loopback.
	r.client = &http.Client{Transport: &http.Transport{DialContext: r.dial, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	var wg sync.WaitGroup
	for sc.Scan() {
		msg := bytes.TrimSpace(sc.Bytes())
		if len(msg) == 0 {
			continue
		}
		msg = bytes.Clone(msg)
		wg.Add(1)
		go func() { // a long tool call must not hold up the others
			defer wg.Done()
			r.forward(ctx, msg)
		}()
	}
	cancel()
	wg.Wait()
	return sc.Err()
}

// live reads the endpoint file of a running Hostbay: missing, damaged or
// naming a process that is gone means not running.
func live(home string) (endpoint, error) {
	e, _, err := readEndpoint(home)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && !alive(e.PID)) {
		return e, ErrNotRunning
	}
	if err != nil {
		return e, fmt.Errorf("%w (%v)", ErrNotRunning, err)
	}
	return e, nil
}

type relay struct {
	home, token string
	client      *http.Client
	log         io.Writer

	mu      sync.Mutex
	out     io.Writer
	version string // the protocol version the server answered initialize or discover with
}

// dial connects to the endpoint file's port and checks, on that same
// connection, that the listener holds this launch's secret.
func (r *relay) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	e, err := live(r.home)
	if err != nil {
		return nil, err
	}
	_, secret, err := readEndpoint(r.home)
	if err != nil {
		return nil, err
	}
	if addr != net.JoinHostPort("127.0.0.1", strconv.Itoa(e.Port)) {
		return nil, errors.New("Hostbay's MCP port changed; send the request again")
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNotRunning, err)
	}
	if err := challenge(c, e.Port, secret); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// challenge asks the listener on c for the HMAC of a fresh nonce. Nothing
// else is written before it answers, and its answer is read to its end.
func challenge(c net.Conn, port int, secret []byte) error {
	var b [32]byte
	_, _ = rand.Read(b[:])
	nonce := hex.EncodeToString(b[:])
	c.SetDeadline(time.Now().Add(5 * time.Second))
	defer c.SetDeadline(time.Time{})
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: 127.0.0.1:%d\r\n%s: %s\r\n\r\n", challengePath, port, challengeHeader, nonce)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return errNotHostbay
	}
	got, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || br.Buffered() > 0 || !hmac.Equal(got, []byte(proof(secret, port, nonce))) {
		return errNotHostbay
	}
	return nil
}

// rpc is the part of a JSON-RPC message the relay reads.
type rpc struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params struct {
		Name string `json:"name"`
		URI  string `json:"uri"`
		Meta map[string]any
	} `json:"params"`
	Result struct {
		ProtocolVersion string `json:"protocolVersion"`
	} `json:"result"`
}

func (r *relay) write(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.out.Write(append(bytes.TrimSpace(b), '\n'))
}

// forward POSTs one message with the headers its protocol version needs
// and writes the reply, or a JSON-RPC error for a call the server refused.
func (r *relay) forward(ctx context.Context, msg []byte) {
	var m rpc
	if err := json.Unmarshal(msg, &m); err != nil {
		fmt.Fprintf(r.log, "hostbay mcp: dropped a message that is not JSON: %v\n", err)
		return
	}
	var meta struct {
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	_ = json.Unmarshal(msg, &meta)
	version, _ := meta.Params.Meta["io.modelcontextprotocol/protocolVersion"].(string)
	r.mu.Lock()
	if version == "" {
		version = r.version
	}
	r.mu.Unlock()

	e, err := live(r.home)
	if err != nil {
		r.refuse(m, err.Error())
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(e.Port))+Path, bytes.NewReader(msg))
	if err != nil {
		r.refuse(m, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	if version != "" {
		req.Header.Set("Mcp-Protocol-Version", version)
	}
	if m.Method != "" {
		req.Header.Set("Mcp-Method", m.Method)
		switch m.Method {
		case "tools/call", "prompts/get":
			req.Header.Set("Mcp-Name", m.Params.Name)
		case "resources/read":
			req.Header.Set("Mcp-Name", m.Params.URI)
		}
	}
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(r.log, "hostbay mcp: %v\n", err)
			r.refuse(m, unwrapDial(err).Error())
		}
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage))
	if err != nil {
		r.refuse(m, err.Error())
		return
	}
	switch {
	case resp.StatusCode == http.StatusAccepted:
	case resp.StatusCode == http.StatusOK && json.Valid(body):
		if m.Method == "initialize" || m.Method == "server/discover" {
			var res rpc
			if json.Unmarshal(body, &res) == nil && res.Result.ProtocolVersion != "" {
				r.mu.Lock()
				r.version = res.Result.ProtocolVersion
				r.mu.Unlock()
			}
		}
		r.write(body)
	case json.Valid(body) && bytes.Contains(body, []byte(`"jsonrpc"`)):
		r.write(body) // the server's own JSON-RPC error
	default:
		r.refuse(m, fmt.Sprintf("Hostbay refused the request: %s: %s", resp.Status, bytes.TrimSpace(body)))
	}
}

// refuse answers a call with a JSON-RPC error; a notification gets none.
func (r *relay) refuse(m rpc, why string) {
	fmt.Fprintf(r.log, "hostbay mcp: %s\n", why)
	if len(m.ID) == 0 || m.Method == "" {
		return
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32001, "message": why}})
	r.write(b)
}

// unwrapDial is the relay's own reason for a failed dial, without the
// HTTP client's wrapping.
func unwrapDial(err error) error {
	for _, e := range []error{errNotHostbay, ErrNotRunning} {
		if errors.Is(err, e) {
			return e
		}
	}
	return err
}
