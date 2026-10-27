package database

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/tlstrust"
)

// rqFake is an rqlite node: it records each request's statements and
// Authorization header and answers with answer.
type rqFake struct {
	*httptest.Server
	mu     sync.Mutex
	stmts  [][]any
	auths  []string
	answer func(w http.ResponseWriter, stmt []any)
}

func newRqFake(t *testing.T, https bool) *rqFake {
	f := &rqFake{answer: func(w http.ResponseWriter, _ []any) {
		w.Write([]byte(`{"results":[{"columns":["1"],"types":["integer"],"values":[[1]]}]}`))
	}}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body [][]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		if len(body) > 0 {
			f.stmts = append(f.stmts, body[0])
		}
		answer := f.answer
		f.mu.Unlock()
		answer(w, body[0])
	})
	if https {
		f.Server = httptest.NewTLSServer(h)
	} else {
		f.Server = httptest.NewServer(h)
	}
	t.Cleanup(f.Close)
	return f
}

func (f *rqFake) cfg(certs *tlstrust.Store) ServerConfig {
	host, port := splitHostPort(f.Listener.Addr().String())
	return ServerConfig{URL: f.URL, User: "app", Password: "rq-Secret-1!",
		Trust: func(try func(*tls.Config) error) (*tls.Config, error) {
			return certs.Capture("rqlite", "h1", host, port, try)
		},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != f.Listener.Addr().String() {
				return nil, errors.New("refused " + addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}
}

func splitHostPort(addr string) (string, int) {
	h, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return h, n
}

// A self-signed node raises the prompt before any request, so the
// credentials never left (KTD25).
func TestRqliteChecksTheCertificateBeforeSigningIn(t *testing.T) {
	f := newRqFake(t, true)
	_, err := OpenRqlite(context.Background(), f.cfg(tlstrust.New(t.TempDir())))
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "rqliteCert" {
		t.Fatalf("err = %v", err)
	}
	if len(f.auths) != 0 {
		t.Fatal("a request reached a node whose certificate was not trusted")
	}
}

// A follower that redirects to its leader is refused, naming the leader,
// and nothing is sent there (KTD24).
func TestRqliteRefusesALeaderRedirect(t *testing.T) {
	f := newRqFake(t, false)
	f.answer = func(w http.ResponseWriter, _ []any) {
		w.Header().Set("Location", "http://10.9.9.9:4001/db/request")
		w.WriteHeader(http.StatusMovedPermanently)
	}
	_, err := OpenRqlite(context.Background(), f.cfg(tlstrust.New(t.TempDir())))
	if err == nil || !strings.Contains(err.Error(), "10.9.9.9:4001") || !strings.Contains(err.Error(), "leader") {
		t.Fatalf("err = %v", err)
	}
	if len(f.stmts) != 1 {
		t.Fatalf("requests = %v", f.stmts)
	}
}

// Statements go with their parameters apart, never spliced into the SQL,
// and with the user's credentials.
func TestRqliteSendsParametersApart(t *testing.T) {
	f := newRqFake(t, false)
	e, err := OpenRqlite(context.Background(), f.cfg(tlstrust.New(t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	f.answer = func(w http.ResponseWriter, _ []any) {
		w.Write([]byte(`{"results":[{"columns":["k","n","d"],"types":["text","text","text"],"values":[["column","id","INTEGER"]]}]}`))
	}
	nodes, _, err := e.Children(context.Background(), []string{`t'); DROP TABLE x; --`}, 0)
	if err != nil || len(nodes) != 1 || nodes[0].Name != "id" || nodes[0].Detail != "INTEGER" {
		t.Fatalf("nodes = %+v, %v", nodes, err)
	}
	last := f.stmts[len(f.stmts)-1]
	if len(last) != 4 || strings.Contains(last[0].(string), "DROP") || last[1] != `t'); DROP TABLE x; --` {
		t.Fatalf("statement = %q", last)
	}
	if !strings.HasPrefix(f.auths[0], "Basic ") {
		t.Fatalf("auth = %q", f.auths[0])
	}
}
