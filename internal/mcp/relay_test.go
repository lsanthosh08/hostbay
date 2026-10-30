package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/mcp/mcptest"
)

// relayRun runs the relay in-process on pipes, as a client's child.
type relayRun struct {
	inW  *io.PipeWriter
	outR *io.PipeReader
	logs *bytes.Buffer
	done chan error
}

func startRelay(home, token string) *relayRun {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	r := &relayRun{inW: inW, outR: outR, logs: &bytes.Buffer{}, done: make(chan error, 1)}
	go func() {
		err := Relay(context.Background(), home, token, inR, outW, r.logs)
		outW.Close()
		r.done <- err
	}()
	return r
}

// line sends one message and returns the relay's next line of output.
func (r *relayRun) line(t *testing.T, msg string) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(r.outR).ReadString('\n')
		got <- s
	}()
	io.WriteString(r.inW, msg+"\n")
	select {
	case s := <-got:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("the relay wrote nothing")
		return ""
	}
}

// With Hostbay running and the token set, the relay forwards initialize
// and tools/list, on both protocol versions.
func TestRelayForwards(t *testing.T) {
	for _, v := range []string{mcptest.V20251125, mcptest.V20260728} {
		t.Run(v, func(t *testing.T) {
			r := newRig(t)
			run := startRelay(r.server, r.token)
			cs, err := mcptest.IO(t.Context(), run.outR, run.inW, v)
			if err != nil {
				t.Fatalf("connect: %v (relay log: %s)", err, run.logs)
			}
			defer cs.Close()
			if got := cs.InitializeResult().ProtocolVersion; got != v {
				t.Fatalf("negotiated %q", got)
			}
			tools, err := cs.ListTools(t.Context(), nil)
			if err != nil || len(tools.Tools) != 12 {
				t.Fatalf("tools = %v, %v", tools, err)
			}
			_, text := call(t, cs, "list_connections", nil)
			if !strings.Contains(text, `"h1"`) {
				t.Fatalf("list_connections = %s", text)
			}
		})
	}
}

// Without the token, the client sees the 401.
func TestRelayWithoutToken(t *testing.T) {
	r := newRig(t)
	run := startRelay(r.server, "")
	out := run.line(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`)
	if !strings.Contains(out, `"id":1`) || !strings.Contains(out, "401") {
		t.Fatalf("out = %q", out)
	}
}

// With Hostbay stopped, the relay exits with a message, dials nothing and
// leaves the lock the store holds untouched.
func TestRelayHostbayStopped(t *testing.T) {
	r := newRig(t)
	r.srv.Stop()
	before, _ := os.ReadDir(r.server)
	err := Relay(context.Background(), r.server, r.token, strings.NewReader(""), io.Discard, io.Discard)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("err = %v", err)
	}
	after, _ := os.ReadDir(r.server)
	if len(before) != len(after) {
		t.Fatalf("the relay changed the default folder: %v -> %v", before, after)
	}
}

// squatter listens where the endpoint file points, records every byte it
// is sent and answers like an HTTP server, with answer as the body.
type squatter struct {
	ln   net.Listener
	mu   sync.Mutex
	seen bytes.Buffer
}

func newSquatter(t *testing.T, answer func(nonce string) string) *squatter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &squatter{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					c.SetReadDeadline(time.Now().Add(2 * time.Second))
					line, err := br.ReadString('\n')
					s.mu.Lock()
					s.seen.WriteString(line)
					s.mu.Unlock()
					if err != nil {
						return
					}
					if line == "\r\n" {
						s.mu.Lock()
						req := s.seen.String()
						s.mu.Unlock()
						nonce := ""
						if i := strings.LastIndex(req, challengeHeader+": "); i >= 0 {
							nonce = strings.TrimSpace(strings.SplitN(req[i+len(challengeHeader)+2:], "\n", 2)[0])
						}
						body := answer(nonce)
						io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
					}
				}
			}()
		}
	}()
	return s
}

func (s *squatter) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *squatter) got() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen.String()
}

// A program that took the endpoint's port while Hostbay was down never
// receives the token: it cannot answer the challenge without the secret,
// whether it makes up a proof or replays one Hostbay gave for another nonce.
func TestRelaySquatterNeverGetsToken(t *testing.T) {
	r := newRig(t)
	_, secret, err := readEndpoint(r.server)
	if err != nil {
		t.Fatal(err)
	}
	r.srv.Stop() // Hostbay quits; the squatter takes over
	for name, answer := range map[string]func(string) string{
		"made up":  func(string) string { return strings.Repeat("ab", 32) },
		"replayed": func(string) string { return proof(secret, r.port, strings.Repeat("00", 32)) },
		"empty":    func(string) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			sq := newSquatter(t, answer)
			// The endpoint file names the squatter's port, with a live PID
			// and the old launch's secret, as after a crash and a restart
			// elsewhere.
			writeEndpoint(r.server, endpoint{PID: os.Getpid(), Port: sq.port(), Secret: base64.StdEncoding.EncodeToString(secret)})
			run := startRelay(r.server, r.token)
			out := run.line(t, `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`)
			if !strings.Contains(out, "not this Hostbay") {
				t.Fatalf("out = %q", out)
			}
			seen := sq.got()
			if !strings.Contains(seen, challengePath) {
				t.Fatalf("the squatter saw no challenge: %q", seen)
			}
			if strings.Contains(seen, r.token) || strings.Contains(seen, "Authorization") || strings.Contains(seen, "initialize") {
				t.Fatalf("the squatter received: %q", seen)
			}
		})
	}
}

// A stale endpoint file is never trusted: a dead PID is not running and
// nothing is dialled; a live listener with another launch's secret gets
// no token.
func TestRelayStaleEndpoint(t *testing.T) {
	r := newRig(t)
	e, secret, _ := readEndpoint(r.server)

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	sq := newSquatter(t, func(string) string { return "" })
	writeEndpoint(r.server, endpoint{PID: cmd.Process.Pid, Port: sq.port(), Secret: base64.StdEncoding.EncodeToString(secret)})
	if err := Relay(context.Background(), r.server, r.token, strings.NewReader(""), io.Discard, io.Discard); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("dead PID: %v", err)
	}
	if sq.got() != "" {
		t.Fatalf("a dead PID's port was dialled: %q", sq.got())
	}

	// The real server on its port, but the file holds another secret.
	other := make([]byte, 32)
	other[0] = 1
	writeEndpoint(r.server, endpoint{PID: os.Getpid(), Port: e.Port, Secret: base64.StdEncoding.EncodeToString(other)})
	tok, _ := r.srv.Tokens.List()
	run := startRelay(r.server, r.token)
	out := run.line(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	if !strings.Contains(out, "not this Hostbay") {
		t.Fatalf("out = %q", out)
	}
	if now, _ := r.srv.Tokens.List(); now[0].LastUsed != tok[0].LastUsed {
		t.Fatal("the token reached a server the relay could not verify")
	}
}

// The endpoint file is 0600 in the default folder while the server runs,
// and gone once it stops.
func TestEndpointFile(t *testing.T) {
	r := newRig(t)
	fi, err := os.Stat(endpointPath(r.server))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("mode %v", fi.Mode())
	}
	if e, _, _ := readEndpoint(r.server); e.Port != r.port || e.PID != os.Getpid() {
		t.Fatalf("endpoint = %+v", e)
	}
	r.srv.Stop()
	if _, err := os.Stat(endpointPath(r.server)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after stop: %v", err)
	}
}
