package app

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/database/dbtest"
	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx/proxytest"
	"hostbay/internal/tlstrust/tlstest"
)

// A host behind a proxy that is down fails promptly as ProxyFailed, the
// code the page answers, whatever the driver does with a failed dial; the
// server sees no connection (KTD24).
func TestRedisMongoDBAndElasticsearchFailAtADeadProxy(t *testing.T) {
	rs, mg := dbtest.NewRedis(t, nil), dbtest.NewMongo(t, nil)
	d, s, _, _ := newDatabaseService(t)
	down, port := proxytest.Down(t)
	dead, err := s.SaveProxy(Proxy{Name: "dead", Type: "http", Host: down, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	for kind, h := range map[string]Host{
		"redis":         redisForm(rs, "none", "", ""),
		"mongodb":       {Alias: "m", Hostname: mg.Host, Port: mg.Port, Kind: new("mongodb"), TLS: new("none")},
		"elasticsearch": {Alias: "e", Kind: new("elasticsearch"), URL: new("http://" + rs.Addr())},
	} {
		h.ProxyID = &dead.ID
		saved, err := s.SaveHost(h)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = d.DatabaseConnect(context.Background(), saved.ID)
		if apperr.CodeOf(err) != apperr.ProxyFailed || time.Since(start) > 5*time.Second {
			t.Errorf("%s: err = %v after %v, want ProxyFailed at once", kind, err, time.Since(start))
		}
	}
	if rs.Conns() != 0 || len(mg.Commands()) != 0 {
		t.Fatal("a server saw a direct connection")
	}
}

// A host behind a jump host whose key is not known yet asks to trust that
// key, as the page needs, and reaches the server through it once trusted.
func TestRedisAndMongoDBAskToTrustTheJumpHost(t *testing.T) {
	rs, mg := dbtest.NewRedis(t, nil), dbtest.NewMongo(t, nil)
	d, s, _, _ := newDatabaseService(t)
	// Each behind its own jump host, so each meets an unknown key.
	bastion := func(target, addr string) *string {
		jump := sftptest.New(t, "jump-pw")
		jump.Resolve(target, addr)
		jhost, jport, _ := net.SplitHostPort(jump.Addr)
		jp, _ := strconv.Atoi(jport)
		auth, jpw := "password", "jump-pw"
		j, err := s.SaveHost(Host{Alias: "bastion " + target, Hostname: jhost, Port: jp, User: "u", AuthType: &auth, Password: &jpw})
		if err != nil {
			t.Fatal(err)
		}
		return &j.ID
	}
	for _, h := range []Host{
		{Alias: "r", Hostname: "cache.internal", Port: rs.Port, Kind: new("redis"), TLS: new("none"), JumpHostID: bastion("cache.internal", rs.Addr())},
		{Alias: "m", Hostname: "mongo.internal", Port: mg.Port, Kind: new("mongodb"), TLS: new("none"), JumpHostID: bastion("mongo.internal", mg.Addr())},
	} {
		saved, err := s.SaveHost(h)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		_, err = d.DatabaseConnect(context.Background(), saved.ID)
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["requestId"] == "" || time.Since(start) > 5*time.Second {
			t.Fatalf("%s: err = %v after %v, want the jump host's key prompt", *h.Kind, err, time.Since(start))
		}
		if err := d.conns.TrustHostKey(ae.Details["requestId"], true); err != nil {
			t.Fatal(err)
		}
		info, err := d.DatabaseConnect(context.Background(), saved.ID)
		if err != nil {
			t.Fatalf("%s through the trusted jump host: %v", *h.Kind, err)
		}
		d.DatabaseClose(info.DatabaseID)
	}
}

func redisForm(srv *dbtest.Redis, tls, user, password string) Host {
	h := Host{Alias: "cache", Hostname: srv.Host, Port: srv.Port, User: user, Kind: new("redis"), TLS: &tls}
	if password != "" {
		h.Password = &password
	}
	return h
}

func sentAuth(srv *dbtest.Redis) []string {
	var out []string
	for _, c := range srv.Commands() {
		if strings.EqualFold(c[0], "AUTH") {
			out = append(out, strings.Join(c[1:], " "))
		}
	}
	return out
}

// A Redis password needs no user name, and with neither Redis signs in to
// nothing instead of asking for a password (R29 still holds: no empty
// password is sent).
func TestRedisHostSignsInWithAPasswordAloneOrNotAtAll(t *testing.T) {
	srv := dbtest.NewRedis(t, nil)
	d, s, _, _ := newDatabaseService(t)
	h, err := s.SaveHost(redisForm(srv, "none", "", dbPassword))
	if err != nil {
		t.Fatal(err)
	}
	info, err := d.DatabaseConnect(context.Background(), h.ID)
	if err != nil {
		t.Fatal(err)
	}
	d.DatabaseClose(info.DatabaseID)
	if got := sentAuth(srv); !slices.Equal(got, []string{dbPassword}) {
		t.Fatalf("AUTH sent %q, want the password alone", got)
	}
	open, err := s.SaveHost(redisForm(srv, "none", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.DatabaseConnect(context.Background(), open.ID); err != nil {
		t.Fatalf("a Redis host with no password asked for one: %v", err)
	}
	if got := sentAuth(srv); len(got) != 1 {
		t.Fatalf("AUTH sent %q, want no second AUTH", got)
	}
}

// A changed Redis certificate is refused with the ID that removes its pin,
// as a PostgreSQL one is.
func TestRedisChangedCertificateOffersPinRemoval(t *testing.T) {
	cert := tlstest.Cert(t)
	srv := dbtest.NewRedis(t, &cert)
	d, s, _, _ := newDatabaseService(t)
	h, err := s.SaveHost(redisForm(srv, "verify", "", dbPassword))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.DatabaseConnect(context.Background(), h.ID)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "redisCert" {
		t.Fatalf("err = %v", err)
	}
	if err := d.DatabaseTrustCert(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DatabaseConnect(context.Background(), h.ID); err != nil {
		t.Fatal(err)
	}
	next := tlstest.Cert(t)
	srv.SetCert(&next)
	_, err = d.DatabaseConnect(context.Background(), h.ID)
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyChanged || ae.Details["removeId"] == "" {
		t.Fatalf("err = %v, want a refusal with a removeId", err)
	}
}

// Settings that could not connect, or would connect another way than the
// form shows, are refused on save.
func TestRedisMongoDBAndElasticsearchSettingsAreChecked(t *testing.T) {
	_, s, _, _ := newDatabaseService(t)
	tls, bad := "verify", "skip"
	for name, h := range map[string]Host{
		"a Redis database that is not a number":    {Alias: "r", Hostname: "cache.lan", Kind: new("redis"), Database: new("one")},
		"a negative Redis database":                {Alias: "r", Hostname: "cache.lan", Kind: new("redis"), Database: new("-1")},
		"an unknown TLS mode":                      {Alias: "m", Hostname: "mongo.lan", Kind: new("mongodb"), TLS: &bad},
		"a MongoDB auth database with a newline":   {Alias: "m", Hostname: "mongo.lan", Kind: new("mongodb"), Database: new("admin\n")},
		"an Elasticsearch URL that is not http(s)": {Alias: "e", Kind: new("elasticsearch"), URL: new("ftp://es.lan")},
		"an Elasticsearch host with a TLS mode":    {Alias: "e", Kind: new("elasticsearch"), URL: new("https://es.lan:9200"), TLS: &tls},
	} {
		if _, err := s.SaveHost(h); err == nil {
			t.Errorf("%s was saved", name)
		}
	}
	if _, err := s.SaveHost(Host{Alias: "e", Kind: new("elasticsearch"), URL: new("https://es.lan:9200")}); err != nil {
		t.Fatalf("a valid Elasticsearch host: %v", err)
	}
}
