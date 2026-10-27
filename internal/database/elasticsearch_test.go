package database

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
)

// fakeES is an Elasticsearch node over TLS with a certificate that can be
// swapped, recording each request's escaped path and whether it carried a
// password.
type fakeES struct {
	srv *httptest.Server

	mu     sync.Mutex
	cert   tls.Certificate
	paths  []string
	authed int
	puts   []string
}

func newFakeES(t *testing.T) *fakeES {
	f := &fakeES{cert: tlstest.Cert(t)}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	// Asked on every handshake, so a swapped certificate serves at once
	// (a GetCertificate would yield to httptest's own without SNI).
	f.srv.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return &tls.Config{Certificates: []tls.Certificate{f.cert}}, nil
	}}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

type esSeen struct {
	paths, puts []string
	authed      int
}

// seen is what the node received so far.
func (f *fakeES) seen() esSeen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return esSeen{paths: slices.Clone(f.paths), puts: slices.Clone(f.puts), authed: f.authed}
}

func (f *fakeES) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.EscapedPath())
	if _, _, ok := r.BasicAuth(); ok {
		f.authed++
	}
	if r.Method == "PUT" {
		f.puts = append(f.puts, r.URL.RawQuery)
	}
	f.mu.Unlock()
	switch p := r.URL.EscapedPath(); {
	case p == "/":
		io.WriteString(w, `{"version":{"number":"8.15.0"}}`)
	case p == "/big":
		io.WriteString(w, `"`+strings.Repeat("x", 2<<20)+`"`)
	case p == "/away":
		http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
	case p == "/a%2Fb/_search":
		io.WriteString(w, `{"hits":{"hits":[{"_index":"a/b","_id":"1","_source":{"n":1}}]}}`)
	case p == "/logs/_doc/7" && r.Method == "GET":
		io.WriteString(w, `{"found":true,"_seq_no":4,"_primary_term":2,"_source":{"msg":"hi"}}`)
	case p == "/logs/_doc/7" && r.Method == "PUT":
		io.WriteString(w, `{"result":"updated"}`)
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"type":"index_not_found_exception","reason":"no such index"}}`)
	}
}

func (f *fakeES) openWith(t *testing.T, certs *tlstrust.Store) (Engine, error) {
	t.Helper()
	addr := f.srv.Listener.Addr().(*net.TCPAddr)
	var dials atomic.Int32
	return OpenElasticsearch(context.Background(), ESConfig{
		URL: f.srv.URL, User: "elastic", Password: "es-Pw-3", Dial: countingDial(addr.String(), &dials),
		Trust: func(try func(*tls.Config) error) (*tls.Config, error) {
			return certs.Capture("elasticsearch", "h1", "127.0.0.1", addr.Port, try)
		},
	})
}

// A self-signed node raises the prompt before the password is sent; once
// trusted it signs in, and a changed certificate is refused.
func TestElasticsearchChecksTheCertificateBeforeThePassword(t *testing.T) {
	f := newFakeES(t)
	certs := tlstrust.New(t.TempDir())
	_, err := f.openWith(t, certs)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "elasticsearchCert" {
		t.Fatalf("err = %v, want the elasticsearchCert prompt", err)
	}
	if f.seen().authed != 0 {
		t.Fatal("the password was sent before the certificate was trusted")
	}
	if err := certs.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	e, err := f.openWith(t, certs)
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	f.mu.Lock()
	f.cert = tlstest.Cert(t)
	f.mu.Unlock()
	f.srv.CloseClientConnections()
	if _, err := f.openWith(t, certs); !errors.As(err, &ae) || ae.Code != apperr.HostKeyChanged {
		t.Fatalf("err = %v, want the changed certificate refused", err)
	}
}

// A ".." segment, plain or escaped, is refused before anything is sent; a
// "/" in an index name stays in its segment; a response is cut at 1 MiB;
// and a redirect to another origin is refused.
// A search response cut at 1 MiB still opens as a document grid of the
// hits that arrived whole, not as raw cut JSON.
func TestElasticsearchCutSearchKeepsTheWholeHits(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"took":1,"hits":{"total":{"value":500},"hits":[`)
	for i := range 500 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"_index":"logs","_id":"%d","_source":{"msg":"%s"}}`, i, strings.Repeat("x", 4<<10))
	}
	b.WriteString(`]}}`)
	resp := []byte(b.String())[:esMaxBody]
	set, ok := esHits(resp, true)
	if !ok {
		t.Fatal("a cut search response showed as raw JSON")
	}
	n := 0
	for {
		row, more, err := set.next()
		if err != nil || !more {
			break
		}
		if row[1] != strconv.Itoa(n) || !json.Valid([]byte(row[2].(string))) {
			t.Fatalf("hit %d = %v", n, row[:2])
		}
		n++
	}
	if n == 0 || n >= 500 {
		t.Fatalf("%d whole hits", n)
	}
	if _, ok := esHits([]byte(`{"hits":{"hits":[{"_id":"1"`), true); ok {
		t.Fatal("a response cut inside its first hit showed as an empty grid")
	}
}

func TestElasticsearchPathsAndResponses(t *testing.T) {
	f := newFakeES(t)
	certs := tlstrust.New(t.TempDir())
	_, err := f.openWith(t, certs)
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatal(err)
	}
	certs.Answer(ae.Details["requestId"], true)
	e, err := f.openWith(t, certs)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx := context.Background()
	before := len(f.seen().paths)
	for _, q := range []string{"GET ../_cluster/settings", "GET /logs/%2e%2e/_cluster/settings", "GET /./x", "PATCH /x", "GET"} {
		if _, err := e.Query(ctx, q); Category(err) != "syntax" {
			t.Fatalf("%q: err = %v", q, err)
		}
	}
	if s := f.seen(); len(s.paths) != before {
		t.Fatalf("refused requests were sent: %q", s.paths[before:])
	}

	ed := e.(DocumentEditor)
	res, err := e.Query(ctx, ed.SelectTable([]string{"a/b"}))
	if err != nil {
		t.Fatal(err)
	}
	if row, ok, _ := res.Next(); !ok || row[1] != "1" || row[2] != `{"n":1}` || res.Columns()[2].Name != "document" {
		t.Fatalf("hit = %v %v", row, ok)
	}

	res, err = e.Query(ctx, "GET /big")
	if err != nil {
		t.Fatal(err)
	}
	row, _, _ := res.Next()
	if s := row[1].(string); len(s) > esMaxBody+100 || !strings.HasSuffix(s, "cut at 1 MiB)") {
		t.Fatalf("response of %d bytes, ends %q", len(s), s[len(s)-30:])
	}

	if _, err := e.Query(ctx, "GET /away"); err == nil || !strings.Contains(err.Error(), "refusing to follow") {
		t.Fatalf("redirect: err = %v", err)
	}

	if err := ed.EditDocument(ctx, []string{"logs"}, "7", `{"msg":"bye"}`); err != nil {
		t.Fatal(err)
	}
	if puts := f.seen().puts; len(puts) != 1 || !strings.Contains(puts[0], "if_seq_no=4") || !strings.Contains(puts[0], "if_primary_term=2") {
		t.Fatalf("PUT queries = %q", puts)
	}
	if err := ed.EditDocument(ctx, []string{"logs"}, "8", `{"msg":"bye"}`); err == nil {
		t.Fatal("an edit of a missing document saved")
	}
	if err := ed.EditDocument(ctx, []string{"logs"}, "7", `[1]`); Category(err) != "syntax" {
		t.Fatalf("a non-object document: err = %v", err)
	}
}
