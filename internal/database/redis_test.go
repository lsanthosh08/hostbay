package database

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/database/dbtest"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
)

// countingDial dials only addr, counting each dial, as the app's
// allowlisted dial function would.
func countingDial(addr string, n *atomic.Int32) Dial {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != addr {
			return nil, fmt.Errorf("refused %s", address)
		}
		n.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
}

func openFakeRedis(t *testing.T, srv *dbtest.Redis, password string, dials *atomic.Int32) Engine {
	t.Helper()
	e, err := OpenRedis(context.Background(), RedisConfig{Host: srv.Host, Port: srv.Port, Password: password, TLS: "none", Dial: countingDial(srv.Addr(), dials)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func sent(srv *dbtest.Redis, name string) bool {
	return slices.ContainsFunc(srv.Commands(), func(c []string) bool { return strings.EqualFold(c[0], name) })
}

// A password with no user name signs in with AUTH <password>; the key
// list pages through SCAN, never KEYS, 200 keys at a time.
func TestRedisSignsInWithAPasswordAndScansKeys(t *testing.T) {
	srv := dbtest.NewRedis(t, nil)
	srv.Keys = 100_000
	var dials atomic.Int32
	e := openFakeRedis(t, srv, "redis-Pw-1", &dials)
	if !slices.ContainsFunc(srv.Commands(), func(c []string) bool { return len(c) == 2 && strings.EqualFold(c[0], "AUTH") && c[1] == "redis-Pw-1" }) {
		t.Fatalf("no AUTH <password>: %q", srv.Commands())
	}
	nodes, more, err := e.Children(context.Background(), nil, 0)
	if err != nil || len(nodes) != TreePage || !more || nodes[0].Name != "k000000" || nodes[0].Detail != "string" {
		t.Fatalf("first page: %d nodes, more %v, err %v, %+v", len(nodes), more, err, nodes[:1])
	}
	next, more, err := e.Children(context.Background(), nil, TreePage)
	if err != nil || len(next) != TreePage || !more || next[0].Name != "k000200" {
		t.Fatalf("second page: %d nodes, more %v, err %v", len(next), more, err)
	}
	if sent(srv, "KEYS") || !sent(srv, "SCAN") {
		t.Fatalf("commands: %q", srv.Commands())
	}
}

// Streaming, blocking-forever and connection-state commands are refused
// before anything is sent, and so is a request holding one.
func TestRedisRefusesStreamingAndConnectionCommands(t *testing.T) {
	srv := dbtest.NewRedis(t, nil)
	var dials atomic.Int32
	e := openFakeRedis(t, srv, "", &dials)
	before := len(srv.Commands())
	for _, q := range []string{"MONITOR", "subscribe news", "PING\nPSUBSCRIBE *", "BLPOP q 0", "BLMPOP 0 1 q LEFT", "XREAD BLOCK 0 STREAMS s $", "SELECT 2", "MULTI", "CLIENT REPLY OFF"} {
		_, err := e.Query(context.Background(), q)
		if err == nil || Category(err) != "syntax" {
			t.Fatalf("%q: err = %v", q, err)
		}
	}
	if len(srv.Commands()) != before {
		t.Fatalf("refused commands reached the server: %q", srv.Commands()[before:])
	}
	if _, err := e.Query(context.Background(), "BLPOP q 5"); err != nil && Category(err) == "syntax" && strings.Contains(err.Error(), "waits forever") {
		t.Fatalf("a blocking pop with a timeout was refused: %v", err)
	}
}

// A Cluster MOVED reply is an error naming where it pointed, and nothing
// is dialled to follow it (KTD24).
func TestRedisMovedIsAnErrorAndDialsNothing(t *testing.T) {
	srv := dbtest.NewRedis(t, nil)
	srv.Replies["GET user:1"] = "-MOVED 3999 10.0.0.9:6379\r\n"
	var dials atomic.Int32
	e := openFakeRedis(t, srv, "", &dials)
	before := dials.Load()
	_, err := e.Query(context.Background(), "GET user:1\nGET other")
	if err == nil || !strings.Contains(err.Error(), "10.0.0.9:6379") || !strings.Contains(err.Error(), "did not follow") {
		t.Fatalf("err = %v", err)
	}
	if dials.Load() != before {
		t.Fatalf("dialled %d more connections", dials.Load()-before)
	}
}

// Against a TLS server the first bytes are a ClientHello, never AUTH. An
// unknown certificate raises the prompt with no AUTH sent; once trusted
// it signs in, and a changed certificate is refused.
func TestRedisTLSChecksTheCertificateBeforeAuth(t *testing.T) {
	cert := tlstest.Cert(t)
	srv := dbtest.NewRedis(t, &cert)
	certs := tlstrust.New(t.TempDir())
	var dials atomic.Int32
	open := func() error {
		e, err := OpenRedis(context.Background(), RedisConfig{
			Host: srv.Host, Port: srv.Port, Password: "redis-Pw-2", TLS: "verify", Dial: countingDial(srv.Addr(), &dials),
			Trust: func(try func(*tls.Config) error) (*tls.Config, error) {
				return certs.Capture("redis", "h1", srv.Host, srv.Port, try)
			},
		})
		if err == nil {
			e.Close()
		}
		return err
	}
	var ae *apperr.Error
	if err := open(); !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "redisCert" {
		t.Fatalf("err = %v, want the redisCert prompt", err)
	}
	if sent(srv, "AUTH") || sent(srv, "HELLO") {
		t.Fatalf("signed in before the certificate was trusted: %q", srv.Commands())
	}
	if err := certs.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	if err := open(); err != nil {
		t.Fatal(err)
	}
	if !sent(srv, "AUTH") {
		t.Fatal("no AUTH after the certificate was trusted")
	}
	if fb := srv.FirstBytes(); len(fb) == 0 || slices.ContainsFunc(fb, func(b byte) bool { return b != 0x16 }) {
		t.Fatalf("first bytes %x, want only TLS handshake records", fb)
	}
	next := tlstest.Cert(t)
	srv.SetCert(&next)
	if err := open(); !errors.As(err, &ae) || ae.Code != apperr.HostKeyChanged {
		t.Fatalf("err = %v, want the changed certificate refused", err)
	}
}

// streamEntries is a raw XRANGE reply of the entries from-0 up to to-0,
// each with one field.
func streamEntries(from, to int) string {
	out := fmt.Sprintf("*%d\r\n", to-from+1)
	for i := from; i <= to; i++ {
		id := fmt.Sprintf("%d-0", i)
		out += fmt.Sprintf("*2\r\n$%d\r\n%s\r\n*2\r\n$1\r\nf\r\n$1\r\nv\r\n", len(id), id)
	}
	return out
}

// setRows reads the rest of res's current result set.
func setRows(t *testing.T, res Results) [][]any {
	t.Helper()
	var rows [][]any
	for {
		row, ok, err := res.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return rows
		}
		rows = append(rows, row)
	}
}

// Opening a stream key reads it a page at a time, never the whole stream
// in one XRANGE reply.
func TestRedisStreamIsReadInPages(t *testing.T) {
	srv := dbtest.NewRedis(t, nil)
	srv.Replies["XRANGE s - + COUNT 500"] = streamEntries(1, 500)
	srv.Replies["XRANGE s 500-1 + COUNT 500"] = streamEntries(501, 501)
	var dials atomic.Int32
	e := openFakeRedis(t, srv, "", &dials)
	res, err := e.Query(context.Background(), "XRANGE s - +")
	if err != nil {
		t.Fatal(err)
	}
	rows := setRows(t, res)
	if len(rows) != 501 || rows[500][0] != `["501-0",["f","v"]]` {
		t.Fatalf("%d rows, last %v", len(rows), rows[len(rows)-1])
	}
	if got := afterStreamID("7-18446744073709551615"); got != "8-0" {
		t.Fatalf("after the last sequence = %s", got)
	}
}

// The commands of a request reach the server in the order typed: a
// whole-value read before a write shows the value the write found.
func TestRedisRunsARequestInOrder(t *testing.T) {
	srv := dbtest.NewRedis(t, nil)
	srv.Replies["HGETALL h"] = "*2\r\n$1\r\nf\r\n$1\r\nv\r\n"
	srv.Replies["hscan h 0 match * count 500"] = "*2\r\n$1\r\n0\r\n*2\r\n$1\r\nf\r\n$1\r\nv\r\n"
	srv.Replies["DEL h"] = ":1\r\n"
	var dials atomic.Int32
	e := openFakeRedis(t, srv, "", &dials)
	res, err := e.Query(context.Background(), "HGETALL h\nDEL h")
	if err != nil {
		t.Fatal(err)
	}
	if rows := setRows(t, res); len(rows) == 0 {
		t.Fatalf("hash rows = %v", rows)
	}
	var order []string
	for _, c := range srv.Commands() {
		if len(c) > 1 && c[1] == "h" {
			order = append(order, c[0])
		}
	}
	if len(order) != 2 || order[1] != "DEL" {
		t.Fatalf("commands on h ran as %q", order)
	}
}

// Arguments split as redis-cli splits them, and a key quoted for a
// command reads back unchanged.
func TestRedisSplitsArgumentsAndQuotesKeys(t *testing.T) {
	args, err := splitRedis(`SET "a b\x41\n" 'it\'s' plain`)
	if err != nil || !slices.Equal(args, []string{"SET", "a bA\n", "it's", "plain"}) {
		t.Fatalf("args = %q, err %v", args, err)
	}
	if _, err := splitRedis(`GET "open`); err == nil {
		t.Fatal("an unclosed quote was accepted")
	}
	for _, k := range []string{"plain", "with space", `q"uote`, `back\slash`, "\x00\xffbin", "", "it's"} {
		got, err := splitRedis("GET " + quoteRedis(k))
		if err != nil || len(got) != 2 || got[1] != k {
			t.Fatalf("%q quoted as %s read back %q (%v)", k, quoteRedis(k), got, err)
		}
	}
}

// A key opens with only its value column editable and no NULL, since
// EditRow refuses the row's own key column and NULL for every type.
func TestRedisKeyOpensWithOnlyItsValueEditable(t *testing.T) {
	srv := dbtest.NewRedis(t, nil)
	want := map[string]string{"string": "value", "hash": "value", "list": "value", "set": "member", "zset": "score"}
	for typ := range want {
		srv.Replies["type "+typ] = "+" + typ + "\r\n"
	}
	var dials atomic.Int32
	ed := openFakeRedis(t, srv, "", &dials).(RowEditor)
	for typ, col := range want {
		tb, err := ed.EditableTable(context.Background(), []string{typ})
		if err != nil || tb == nil || !tb.NoNull || !slices.Equal(tb.Edit, []string{col}) {
			t.Fatalf("%s opens as %+v, %v; want only %s editable, no NULL", typ, tb, err, col)
		}
	}
}
