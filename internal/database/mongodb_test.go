package database

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"hostbay/internal/apperr"
	"hostbay/internal/database/dbtest"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
)

// addrLog records every address the driver asked to dial, dialling only
// the host's own.
type addrLog struct {
	mu    sync.Mutex
	addrs []string
}

func (l *addrLog) dial(want string) Dial {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		l.mu.Lock()
		l.addrs = append(l.addrs, address)
		l.mu.Unlock()
		if address != want {
			return nil, errors.New("refused " + address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
}

func (l *addrLog) all() []string { l.mu.Lock(); defer l.mu.Unlock(); return slices.Clone(l.addrs) }

// A server whose hello lists another replica-set member is reached by a
// direct connection: every connection, monitoring ones included, goes
// through the dial function to the host itself, and the member is never
// dialled (KTD24).
func TestMongoDirectConnectionDialsOnlyTheHost(t *testing.T) {
	srv := dbtest.NewMongo(t, nil)
	var log addrLog
	e, err := OpenMongo(context.Background(), MongoConfig{Host: srv.Host, Port: srv.Port, TLS: "none", Dial: log.dial(srv.Addr())})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	nodes, _, err := e.Children(context.Background(), nil, 0)
	if err != nil || len(nodes) != 1 || nodes[0].Name != "app" || !nodes[0].HasChildren {
		t.Fatalf("nodes = %+v, err %v", nodes, err)
	}
	time.Sleep(300 * time.Millisecond) // a replica-set client would be dialling the member by now
	addrs := log.all()
	if len(addrs) < 2 || slices.ContainsFunc(addrs, func(a string) bool { return a != srv.Addr() }) {
		t.Fatalf("dialled %q, want only %s, monitor and pool alike", addrs, srv.Addr())
	}
}

// Over TLS the first bytes are a ClientHello; an unknown certificate
// raises the prompt promptly with no command, sign-in included, sent; once
// trusted the sign-in is reached and refused as an auth error.
func TestMongoTLSChecksTheCertificateBeforeSignIn(t *testing.T) {
	cert := tlstest.Cert(t)
	srv := dbtest.NewMongo(t, &cert)
	certs := tlstrust.New(t.TempDir())
	var log addrLog
	open := func() error {
		e, err := OpenMongo(context.Background(), MongoConfig{
			Host: srv.Host, Port: srv.Port, User: "app", Password: "mongo-Pw-4", TLS: "verify", Dial: log.dial(srv.Addr()),
			Trust: func(try func(*tls.Config) error) (*tls.Config, error) {
				return certs.Capture("mongodb", "h1", srv.Host, srv.Port, try)
			},
		})
		if err == nil {
			e.Close()
		}
		return err
	}
	start := time.Now()
	err := open()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "mongodbCert" {
		t.Fatalf("err = %v, want the mongodbCert prompt", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("the prompt took %v", time.Since(start))
	}
	if cmds := srv.Commands(); len(cmds) != 0 {
		t.Fatalf("commands before the certificate was trusted: %q", cmds)
	}
	if err := certs.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	if err := open(); Category(err) != "auth" {
		t.Fatalf("err = %v, want the fake's refused sign-in", err)
	}
	if !slices.Contains(srv.Commands(), "saslStart") && !slices.ContainsFunc(srv.Commands(), func(c string) bool { return strings.EqualFold(c, "hello") || c == "isMaster" }) {
		t.Fatalf("no sign-in after trust: %q", srv.Commands())
	}
	if fb := srv.FirstBytes(); slices.ContainsFunc(fb, func(b byte) bool { return b != 0x16 }) {
		t.Fatalf("first bytes %x, want only TLS handshake records", fb)
	}
}

// A document edit replaces the document with a $literal, so a field named
// "$set" is stored, not run; its _id stays the one it was opened by.
func TestMongoEditReplacesWithALiteral(t *testing.T) {
	var d bson.D
	if err := bson.UnmarshalExtJSON([]byte(`{"$set": {"a": 1}, "n": {"$numberLong": "5"}}`), false, &d); err != nil {
		t.Fatal(err)
	}
	f, err := mongoFilter(`{"$oid": "65a1b2c3d4e5f60718293a4b"}`)
	if err != nil {
		t.Fatal(err)
	}
	d, err = withID(d, f[0].Value)
	if err != nil || d[0].Key != "_id" {
		t.Fatalf("doc = %v, err %v", d, err)
	}
	b, err := bson.MarshalExtJSON(bson.D{{Key: "u", Value: replaceLiteral(d)}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"u":[{"$replaceWith":{"$literal":{"_id":{"$oid":"65a1b2c3d4e5f60718293a4b"},"$set":{"a":{"$numberInt":"1"}},"n":{"$numberLong":"5"}}}}]}`
	if string(b) != want {
		t.Fatalf("update = %s\nwant     %s", b, want)
	}
	other, _ := mongoFilter(`"65a1b2c3d4e5f60718293a4b"`)
	if _, err := withID(d, other[0].Value); err == nil {
		t.Fatal("an edit changed the document's _id from an ObjectId to a string")
	}
	for _, bad := range []string{`1, "$where": "x"`, `{"$gt": 1}}, {"x": 1`, `nope`} {
		if f, err := mongoFilter(bad); err == nil && len(f) == 1 {
			if _, isDoc := f[0].Value.(bson.D); !isDoc {
				t.Fatalf("%q parsed as an _id filter: %v", bad, f)
			}
		}
	}
}

// An _id that is a query operator, such as {"$ne": null}, would match some
// other document: Document would show it and an edit would aim at it.
// Extended JSON types ($oid, $numberLong) parse to values, not documents,
// so they still name one _id, as does a plain embedded document.
func TestMongoFilterRefusesOperators(t *testing.T) {
	for _, bad := range []string{`{"$ne": null}`, `{"$gt": {"$minKey": 1}}`, `{"a": 1, "$or": []}`} {
		if _, err := mongoFilter(bad); err == nil {
			t.Errorf("%s was taken as an _id", bad)
		}
	}
	for _, good := range []string{`{"$oid": "65a1b2c3d4e5f60718293a4b"}`, `{"$numberLong": "5"}`, `{"a": 1, "b": "x"}`, `"s"`} {
		if _, err := mongoFilter(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
}

// A save replaces the document only while it is still the one the page
// was shown: the filter holds the whole current document, so the check
// and the replace are one step on the server.
func TestMongoSaveIsConditionalOnTheShownVersion(t *testing.T) {
	cur, _ := bson.Marshal(bson.D{{Key: "_id", Value: int32(3)}, {Key: "n", Value: int64(3)}})
	f, err := mongoFilter(`{"$numberInt":"3"}`)
	if err != nil {
		t.Fatal(err)
	}
	b, err := bson.MarshalExtJSON(mongoUnchanged(f, cur), true, false)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"_id":{"$numberInt":"3"},"$expr":{"$eq":["$$ROOT",{"$literal":{"_id":{"$numberInt":"3"},"n":{"$numberLong":"3"}}}]}}`
	if string(b) != want {
		t.Fatalf("filter = %s\nwant     %s", b, want)
	}
	other, _ := bson.Marshal(bson.D{{Key: "_id", Value: int32(3)}, {Key: "n", Value: int64(4)}})
	if mongoVersion(cur) == mongoVersion(other) || mongoVersion(cur) != mongoVersion(bson.Raw(slices.Clone(cur))) {
		t.Fatal("the version does not follow the document's content")
	}
}

// A collection browse parses its JSON filter and sort in Go into the
// command's own fields, so a filter cannot add to the command; bad JSON
// and column filters are refused.
func TestMongoBrowseCommand(t *testing.T) {
	path := []string{"shop", "orders"}
	cmd, err := mongoBrowse(path, Browse{Query: `{"status": "open", "find": "users"}`, Order: `{"total": -1}`}, false)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := bson.MarshalExtJSON(cmd, false, false)
	if want := `{"find":"orders","filter":{"status":"open","find":"users"},"sort":{"total":-1}}`; string(got) != want {
		t.Fatalf("find = %s, want %s", got, want)
	}
	cmd, _ = mongoBrowse(path, Browse{Sort: "total", Desc: true}, false)
	if got, _ := bson.MarshalExtJSON(cmd, false, false); string(got) != `{"find":"orders","filter":{},"sort":{"total":-1}}` {
		t.Fatalf("header sort = %s", got)
	}
	cmd, _ = mongoBrowse(path, Browse{Query: `{"_id": {"$oid": "5f0000000000000000000001"}}`}, true)
	if got, _ := bson.MarshalExtJSON(cmd, false, false); string(got) != `{"count":"orders","query":{"_id":{"$oid":"5f0000000000000000000001"}}}` {
		t.Fatalf("count = %s", got)
	}
	for _, b := range []Browse{
		{Query: `{"status": `},
		{Query: `["status"]`},
		{Order: `nope`},
		{Filters: []Filter{{Column: "status", Op: "equals", Value: "open"}}},
	} {
		if _, err := mongoBrowse(path, b, false); Category(err) != "syntax" {
			t.Errorf("%+v: err = %v", b, err)
		}
	}
	if _, err := mongoBrowse([]string{"shop"}, Browse{}, false); err == nil {
		t.Error("a database browsed as a collection")
	}
}
