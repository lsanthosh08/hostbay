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
