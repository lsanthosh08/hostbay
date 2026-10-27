package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestProxiesPersistWithMetaAndLeaveTombstones(t *testing.T) {
	s := openTestStore(t)
	var id string
	d := edit(t, s, func(d *Data) error {
		id = d.PutProxy(Proxy{Name: "office", Type: "socks5", Host: "proxy.example", Port: 1080, User: "u", Password: "enc:v1:x"}).ID
		return nil
	})
	got := d.Proxy(id)
	if got == nil || got.Revision != 1 || got.DeviceID != s.DeviceID() || got.Password != "enc:v1:x" {
		t.Fatalf("saved proxy = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "proxies.json")); err != nil {
		t.Fatalf("proxies.json not written: %v", err)
	}

	d = edit(t, s, func(d *Data) error { return d.PurgeProxy(id) })
	if d.Proxy(id) != nil || len(d.Proxies.Tombstones) != 1 || d.Proxies.Tombstones[0].Revision != 2 {
		t.Fatalf("after purge: %+v", d.Proxies)
	}
}

// A proxy's password is bound to where it is sent, like a host's (KTD6).
func TestProxySecretAADFollowsTheAddress(t *testing.T) {
	p := Proxy{Meta: Meta{ID: "p1"}, Type: "http", Host: "a", Port: 3128, User: "u"}
	q := p
	q.Host = "b"
	r := p
	r.Type = "socks5"
	if bytes.Equal(p.SecretAAD(), q.SecretAAD()) || bytes.Equal(p.SecretAAD(), r.SecretAAD()) {
		t.Fatal("moving the proxy or changing its type kept the binding")
	}
}
