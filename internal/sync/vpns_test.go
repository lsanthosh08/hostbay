package sync

import (
	"encoding/json"
	"errors"
	"testing"

	"hostbay/internal/store"
)

// addVPN saves a profile on d with its secrets sealed there.
func (d *device) addVPN(t *testing.T, gateway, privateKey string) string {
	t.Helper()
	var id string
	d.edit(t, func(data *store.Data) error {
		v := store.VPN{Meta: store.Meta{ID: store.NewID()}, Name: "office", Type: "wireguard", Tier: store.VPNTierApp, Gateway: gateway, Port: 51820, User: "vu", Tags: []string{"project-a"}}
		raw, _ := json.Marshal(store.VPNSecrets{PrivateKey: privateKey})
		sealed, err := d.vault.Seal(raw, v.SecretAAD())
		v.Secret = sealed
		id = data.PutVPN(v).ID
		return err
	})
	return id
}

func (d *device) vpnKey(t *testing.T, id string) (string, error) {
	t.Helper()
	v := d.data(t).VPN(id)
	if v == nil {
		return "", errors.New("no profile")
	}
	raw, err := d.vault.Open(v.Secret, v.SecretAAD())
	if err != nil {
		return "", err
	}
	var s store.VPNSecrets
	err = json.Unmarshal(raw, &s)
	return s.PrivateKey, err
}

// VPN profiles sync as their own collection, their sealed secrets
// re-wrapped so they open on the other computer (R5, KTD4).
func TestVPNsSyncWithTheirSecret(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	vid := a.addVPN(t, "vpn.example", "test-wg-key")
	a.sync(t)
	b.sync(t)
	if k, err := b.vpnKey(t, vid); err != nil || k != "test-wg-key" {
		t.Fatalf("VPN secret on B = %q, %v", k, err)
	}
	if v := b.data(t).VPN(vid); v == nil || len(v.Tags) != 1 || v.Tags[0] != "project-a" {
		t.Fatalf("VPN on B = %+v", v)
	}
	a.edit(t, func(d *store.Data) error { return d.PurgeVPN(vid) })
	a.sync(t)
	b.sync(t)
	if b.data(t).VPN(vid) != nil {
		t.Fatal("a purged profile came back")
	}
}

// Pointing a synced profile at another gateway, switching its type or
// moving its tier waits for confirmation: until then this computer keeps
// the profile it confirmed, and the secret, sealed for the new one, does
// not open for it.
func TestSyncedVPNGatewayTypeOrTierChangeWaitsForConfirmation(t *testing.T) {
	for name, change := range map[string]func(v *store.VPN){
		"gateway": func(v *store.VPN) { v.Gateway = "evil.example" },
		"type":    func(v *store.VPN) { v.Type = "openvpn" },
		"tier":    func(v *store.VPN) { v.Tier = store.VPNTierSystem },
	} {
		t.Run(name, func(t *testing.T) {
			remote := &memBackend{}
			a, b := newDevice(t, remote), newDevice(t, remote)
			vid := a.addVPN(t, "vpn.example", "test-wg-key")
			a.sync(t)
			b.sync(t)
			before := *b.data(t).VPN(vid)
			a.edit(t, func(d *store.Data) error {
				v := *d.VPN(vid)
				change(&v)
				raw, _ := json.Marshal(store.VPNSecrets{PrivateKey: "test-new-key"})
				sealed, err := a.vault.Seal(raw, v.SecretAAD())
				v.Secret = sealed
				d.PutVPN(v)
				return err
			})
			a.sync(t)
			b.sync(t)
			b.sync(t) // a second pass must not apply it either
			v := b.data(t).VPN(vid)
			if v.Gateway != before.Gateway || v.Type != before.Type || v.Tier != before.Tier || len(b.eng.State.Holds) != 1 || b.eng.State.Holds[0].Kind != "vpn" {
				t.Fatalf("VPN on B = %+v, holds %+v", v, b.eng.State.Holds)
			}
			if _, err := b.vpnKey(t, vid); err == nil {
				t.Fatal("the new secret opened for the confirmed profile")
			}
			a.sync(t)
			if v := a.data(t).VPN(vid); v.Gateway == before.Gateway && v.Type == before.Type && v.Tier == before.Tier {
				t.Fatalf("B's hold changed the profile on A: %+v", v)
			}

			if err := b.eng.ResolveHold(vid, true); err != nil {
				t.Fatal(err)
			}
			want := *b.data(t).VPN(vid)
			change(&before)
			if want.Gateway != before.Gateway || want.Type != before.Type || want.Tier != before.Tier || len(b.eng.State.Holds) != 0 {
				t.Fatalf("after confirming: %+v, holds %+v", want, b.eng.State.Holds)
			}
			if k, err := b.vpnKey(t, vid); err != nil || k != "test-new-key" {
				t.Fatalf("secret after confirming = %q, %v", k, err)
			}
		})
	}
}

// A synced change of a host's VPN waits for confirmation like its proxy: a
// host known here keeps its confirmed VPN, one new here gets HeldVPNID and
// cannot connect at all. A VNC host is used so a VPN-only hold is never
// mistaken for an older plaintext hold, which would make it plain on
// confirming. Editing the held host here never uploads the stand-in.
func TestSyncedHostVPNWaitsForConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	vid := a.addVPN(t, "vpn.example", "test-wg-key")
	vnc := "vnc"
	var known string
	a.edit(t, func(d *store.Data) error {
		known = d.PutHost(store.Host{Alias: "desk", Hostname: "desk.example", Port: 5900, Kind: &vnc}).ID
		return nil
	})
	a.sync(t)
	b.sync(t)
	fresh := ""
	a.edit(t, func(d *store.Data) error {
		fresh = d.PutHost(store.Host{Alias: "inner", Hostname: "10.10.0.5", Port: 22, VPNID: &vid}).ID
		return d.EditHost(known, func(h *store.Host) { h.VPNID = &vid })
	})
	a.sync(t)
	b.sync(t)
	b.sync(t)
	if h := b.host(t, known); h.VPNID != nil {
		t.Fatalf("known host on B = %+v; want its confirmed (no) VPN", h)
	}
	if h := b.host(t, fresh); h.VPNID == nil || *h.VPNID != store.HeldVPNID {
		t.Fatalf("new host on B = %+v; want HeldVPNID", h)
	}
	if len(b.eng.State.Holds) != 2 {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
	for _, hd := range b.eng.State.Holds {
		if hd.Kind != "host" || hd.VPNID == nil || *hd.VPNID != vid || hd.Plaintext {
			t.Fatalf("hold = %+v", hd)
		}
	}

	b.edit(t, func(d *store.Data) error {
		return d.EditHost(fresh, func(h *store.Host) { h.Alias = "inner-renamed" })
	})
	b.sync(t)
	a.sync(t)
	if h := a.host(t, fresh); h.Alias != "inner-renamed" || h.VPNID == nil || *h.VPNID != vid {
		t.Fatalf("host on A = %+v; the stand-in travelled", h)
	}

	if err := b.eng.ResolveHold(known, true); err != nil {
		t.Fatal(err)
	}
	if h := b.host(t, known); h.VPNID == nil || *h.VPNID != vid || h.VNCSecurity() != "vencrypt" {
		t.Fatalf("after confirming: %+v", h)
	}
	if err := b.eng.ResolveHold(fresh, false); err != nil {
		t.Fatal(err)
	}
	if h := b.host(t, fresh); h.VPNID != nil {
		t.Fatalf("after refusing: %+v; HeldVPNID must not travel", h)
	}
}
