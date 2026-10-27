package mcp

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func post(t *testing.T, r *rig, mutate func(*http.Request)) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, r.url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	req.Header.Set("Authorization", "Bearer "+r.token)
	mutate(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("the server sent a CORS header")
	}
	return resp.StatusCode
}

// No token or a wrong one gets 401; any Origin, including null, and a
// Host that is not this listener get 403, so a page reached by DNS
// rebinding cannot call the server.
func TestMiddleware(t *testing.T) {
	r := newRig(t)
	port := strconv.Itoa(r.port)
	cases := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"valid", func(*http.Request) {}, http.StatusOK},
		{"localhost host", func(q *http.Request) { q.Host = "localhost:" + port }, http.StatusOK},
		{"no token", func(q *http.Request) { q.Header.Del("Authorization") }, http.StatusUnauthorized},
		{"wrong token", func(q *http.Request) { q.Header.Set("Authorization", "Bearer hbmcp_wrong") }, http.StatusUnauthorized},
		{"evil origin", func(q *http.Request) { q.Header.Set("Origin", "http://evil.example") }, http.StatusForbidden},
		{"null origin", func(q *http.Request) { q.Header.Set("Origin", "null") }, http.StatusForbidden},
		{"empty origin", func(q *http.Request) { q.Header["Origin"] = []string{""} }, http.StatusForbidden},
		{"own origin", func(q *http.Request) { q.Header.Set("Origin", "http://127.0.0.1:"+port) }, http.StatusForbidden},
		{"rebound host", func(q *http.Request) { q.Host = "evil.example:" + port }, http.StatusForbidden},
		{"other port", func(q *http.Request) { q.Host = "127.0.0.1:1" }, http.StatusForbidden},
	}
	for _, c := range cases {
		if got := post(t, r, c.mutate); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

// The listener is on loopback only: the computer's other addresses do not
// answer on its port.
func TestListenerLoopbackOnly(t *testing.T) {
	r := newRig(t)
	addrs, _ := net.InterfaceAddrs()
	tried := false
	for _, a := range addrs {
		ip, _, _ := net.ParseCIDR(a.String())
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		tried = true
		c, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(r.port)), time.Second)
		if err == nil {
			c.Close()
			t.Fatalf("the server answers on %s", ip)
		}
	}
	if !tried {
		t.Skip("this computer has no address but loopback")
	}
}

// A token holding more than its share of open requests is refused.
func TestInFlightCap(t *testing.T) {
	block := make(chan struct{})
	h := capInFlight(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	r := newRig(t)
	tokens := r.srv.Tokens
	srv := &http.Server{Handler: bearer(tokens, h)}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	defer srv.Close()
	codes := make(chan int, maxInFlight+1)
	for range maxInFlight + 1 {
		go func() {
			req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/", nil)
			req.Header.Set("Authorization", "Bearer "+r.token)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
				codes <- resp.StatusCode
			}
		}()
	}
	select {
	case c := <-codes:
		if c != http.StatusTooManyRequests {
			t.Fatalf("first answer %d, want 429", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no request was refused")
	}
	close(block)
}
