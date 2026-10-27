package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTunnelsPersistWithMetaAndLeaveTombstones(t *testing.T) {
	s := openTestStore(t)
	var id string
	d := edit(t, s, func(d *Data) error {
		id = d.PutTunnel(Tunnel{HostID: "h1", Name: "db", Kind: "local", BindPort: 5432, TargetHost: "127.0.0.1", TargetPort: 5432}).ID
		return nil
	})
	got := d.Tunnel(id)
	if got == nil || got.Revision != 1 || got.DeviceID != s.DeviceID() || got.Name != "db" {
		t.Fatalf("saved tunnel = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "tunnels.json")); err != nil {
		t.Fatalf("tunnels.json not written: %v", err)
	}

	d = edit(t, s, func(d *Data) error {
		tn := *d.Tunnel(id)
		tn.BindPort = 15432
		d.PutTunnel(tn)
		return nil
	})
	if tn := d.Tunnel(id); tn.BindPort != 15432 || tn.Revision != 2 {
		t.Fatalf("edited tunnel = %+v", tn)
	}

	d = edit(t, s, func(d *Data) error { return d.PurgeTunnel(id) })
	if d.Tunnel(id) != nil {
		t.Fatal("purged tunnel still listed")
	}
	if ts := d.Tunnels.Tombstones; len(ts) != 1 || ts[0].ID != id || !ts[0].Deleted || ts[0].Revision != 3 {
		t.Fatalf("tombstones = %+v", ts)
	}
	if err := s.Edit(func(d *Data) error { return d.PurgeTunnel(id) }); err == nil {
		t.Fatal("purging a missing tunnel succeeded")
	}
}
