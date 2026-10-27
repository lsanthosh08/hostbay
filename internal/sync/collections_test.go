package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/store"
)

// addProxy saves a proxy on d with its password sealed there.
func (d *device) addProxy(t *testing.T, host, password string) string {
	t.Helper()
	var id string
	d.edit(t, func(data *store.Data) error {
		p := store.Proxy{Meta: store.Meta{ID: store.NewID()}, Name: "office", Type: "socks5", Host: host, Port: 1080, User: "pu"}
		sealed, err := d.vault.Seal([]byte(password), p.SecretAAD())
		p.Password = sealed
		id = data.PutProxy(p).ID
		return err
	})
	return id
}

func (d *device) proxyPassword(t *testing.T, id string) (string, error) {
	t.Helper()
	p := d.data(t).Proxy(id)
	pw, err := d.vault.Open(p.Password, p.SecretAAD())
	return string(pw), err
}

// Proxies sync as their own collection, their sealed password re-wrapped
// so it opens on the other computer (KTD22).
func TestProxiesSyncWithTheirPassword(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	pid := a.addProxy(t, "proxy.example", "proxy-pw")
	a.sync(t)
	b.sync(t)
	if pw, err := b.proxyPassword(t, pid); err != nil || pw != "proxy-pw" {
		t.Fatalf("proxy password on B = %q, %v", pw, err)
	}
	a.edit(t, func(d *store.Data) error { return d.PurgeProxy(pid) })
	a.sync(t)
	b.sync(t)
	if b.data(t).Proxy(pid) != nil {
		t.Fatal("a purged proxy came back")
	}
}

// A synced change that turns on agent forwarding or legacy algorithms, or
// that sets a host's proxy, applies on a computer only after confirmation
// there. Meanwhile the host forwards nothing, offers only the defaults and
// cannot connect at all rather than skip the proxy (KTD22).
func TestSyncedSecurityWeakeningHostSettingsWaitForConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	pid := a.addProxy(t, "proxy.example", "proxy-pw")
	legacy := &store.Algorithms{KeyExchanges: []string{ssh.InsecureKeyExchangeDH1SHA1}}
	hid := a.addHost(t, "web", "")
	on := true
	a.edit(t, func(d *store.Data) error {
		return d.EditHost(hid, func(h *store.Host) { h.ForwardAgent, h.Algorithms, h.ProxyID = &on, legacy, &pid })
	})
	a.sync(t)
	b.sync(t)
	b.sync(t) // a second pass must not apply it either
	h := b.host(t, hid)
	if h.Forwards() || h.Algorithms != nil || h.ProxyID == nil || *h.ProxyID != store.HeldProxyID {
		t.Fatalf("held host on B = %+v", h)
	}
	if len(b.eng.State.Holds) != 1 {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
	if hd := b.eng.State.Holds[0]; hd.Kind != "host" || !hd.ForwardAgent || hd.Algorithms == nil || hd.ProxyID == nil || hd.Plaintext {
		t.Fatalf("hold = %+v", hd)
	}
	a.sync(t)
	if h := a.host(t, hid); !h.Forwards() || h.Algorithms == nil || *h.ProxyID != pid {
		t.Fatalf("B's hold changed the host on A: %+v", h)
	}

	if err := b.eng.ResolveHold(hid, true); err != nil {
		t.Fatal(err)
	}
	h = b.host(t, hid)
	if !h.Forwards() || !reflect.DeepEqual(h.Algorithms, legacy) || *h.ProxyID != pid || len(b.eng.State.Holds) != 0 {
		t.Fatalf("after confirming: %+v", h)
	}

	// The same settings changed here are this computer's own: no hold.
	b.edit(t, func(d *store.Data) error {
		return d.EditHost(hid, func(h *store.Host) { h.ProxyID = nil })
	})
	b.sync(t)
	if len(b.eng.State.Holds) != 0 {
		t.Fatalf("a local change was held: %+v", b.eng.State.Holds)
	}
}

// X11 forwarding turned on from another computer lets the host see this
// computer's display, so it waits for confirmation here; until then the
// host forwards nothing (KTD20). Refusing it reaches the other computer.
func TestSyncedX11ForwardingWaitsForConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	hid := a.addHost(t, "desk", "")
	a.sync(t)
	b.sync(t)
	on := true
	a.edit(t, func(d *store.Data) error {
		return d.EditHost(hid, func(h *store.Host) { h.X11 = &on })
	})
	a.sync(t)
	b.sync(t)
	b.sync(t)
	if h := b.host(t, hid); h.ForwardsX11() {
		t.Fatal("synced X11 forwarding applied without confirmation")
	}
	if len(b.eng.State.Holds) != 1 || !b.eng.State.Holds[0].X11 || b.eng.State.Holds[0].Plaintext {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
	a.sync(t)
	if !a.host(t, hid).ForwardsX11() {
		t.Fatal("B's hold turned X11 off on A")
	}

	if err := b.eng.ResolveHold(hid, false); err != nil {
		t.Fatal(err)
	}
	b.sync(t)
	a.sync(t)
	if a.host(t, hid).ForwardsX11() || b.host(t, hid).ForwardsX11() {
		t.Fatal("refusing kept X11 forwarding on")
	}

	// Accepting applies the synced setting.
	a.edit(t, func(d *store.Data) error {
		return d.EditHost(hid, func(h *store.Host) { h.X11 = &on })
	})
	a.sync(t)
	b.sync(t)
	if err := b.eng.ResolveHold(hid, true); err != nil {
		t.Fatal(err)
	}
	if !b.host(t, hid).ForwardsX11() || len(b.eng.State.Holds) != 0 {
		t.Fatal("confirming did not turn X11 forwarding on")
	}
}

// Pointing a synced proxy somewhere else waits for confirmation: until
// then this computer keeps dialling the address it confirmed.
func TestSyncedProxyAddressChangeWaitsForConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	pid := a.addProxy(t, "proxy.example", "proxy-pw")
	a.sync(t)
	b.sync(t)
	a.edit(t, func(d *store.Data) error {
		p := *d.Proxy(pid)
		p.Host = "evil.example"
		d.PutProxy(p)
		return nil
	})
	a.sync(t)
	b.sync(t)
	b.sync(t)
	if p := b.data(t).Proxy(pid); p.Host != "proxy.example" || len(b.eng.State.Holds) != 1 || b.eng.State.Holds[0].Kind != "proxy" {
		t.Fatalf("proxy on B = %+v, holds %+v", p, b.eng.State.Holds)
	}
	if err := b.eng.ResolveHold(pid, true); err != nil {
		t.Fatal(err)
	}
	if p := b.data(t).Proxy(pid); p.Host != "evil.example" {
		t.Fatalf("after confirming: %+v", p)
	}
}

// An upload keeps a field only a newer Hostbay knows from the target's
// copy, but not one this build knows and cleared, such as X11 forwarding
// turned off.
func TestOverlayKeepsOnlyUnknownFields(t *testing.T) {
	hosts := collections[0]
	got := overlay(json.RawMessage(`{"id":"h","x11":true,"forwardAgent":true,"fromTheFuture":1}`), json.RawMessage(`{"id":"h"}`), hosts.known)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["fromTheFuture"]; !ok || m["x11"] != nil || m["forwardAgent"] != nil {
		t.Fatalf("overlay = %s", got)
	}
}

// Picking "merge" on a first sync does not confirm the other computer's
// security settings: its hosts arrive held, like any host new here.
func TestFirstSyncMergeHoldsRemoteWeakenedSettings(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	hid := a.addHost(t, "desk", "")
	on := true
	a.edit(t, func(d *store.Data) error {
		return d.EditHost(hid, func(h *store.Host) { h.ForwardAgent, h.X11 = &on, &on })
	})
	a.sync(t)
	b.addHost(t, "local", "")
	if _, err := b.eng.Sync(context.Background(), "merge"); err != nil {
		t.Fatal(err)
	}
	if h := b.host(t, hid); h == nil || h.Forwards() || h.ForwardsX11() {
		t.Fatalf("merged host on B = %+v", h)
	}
	if len(b.eng.State.Holds) != 1 || !b.eng.State.Holds[0].ForwardAgent || !b.eng.State.Holds[0].X11 {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
}

// A synced switch of a VNC host from VeNCrypt to plain waits for
// confirmation: meanwhile the host keeps VeNCrypt, and refusing sends
// VeNCrypt back rather than the plain setting (KTD18).
func TestSyncedPlainVNCWaitsForConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	vnc, none := "vnc", "none"
	var hid string
	a.edit(t, func(d *store.Data) error {
		hid = d.PutHost(store.Host{Alias: "desk", Hostname: "desk.example", Port: 5900, Kind: &vnc}).ID
		return nil
	})
	a.sync(t)
	b.sync(t)
	a.edit(t, func(d *store.Data) error {
		return d.EditHost(hid, func(h *store.Host) { h.TLS = &none })
	})
	a.sync(t)
	b.sync(t)
	b.sync(t)
	if h := b.host(t, hid); h.VNCSecurity() != "vencrypt" {
		t.Fatalf("synced plain VNC applied before confirmation: %+v", h)
	}
	if len(b.eng.State.Holds) != 1 || !b.eng.State.Holds[0].Plaintext {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
	if err := b.eng.ResolveHold(hid, false); err != nil {
		t.Fatal(err)
	}
	b.sync(t)
	a.sync(t)
	if h := a.host(t, hid); h.VNCSecurity() != "vencrypt" {
		t.Fatalf("refusal did not reach A: %+v", h)
	}
}

// Editing a held host here before answering the hold never uploads the
// HeldProxyID stand-in: the other computers keep the synced proxy and
// this computer's hold stays to be answered.
func TestEditedHeldHostDoesNotUploadTheStandIn(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	pid := a.addProxy(t, "proxy.example", "proxy-pw")
	hid := a.addHost(t, "web", "")
	a.edit(t, func(d *store.Data) error {
		return d.EditHost(hid, func(h *store.Host) { h.ProxyID = &pid })
	})
	a.sync(t)
	b.sync(t)
	b.edit(t, func(d *store.Data) error {
		return d.EditHost(hid, func(h *store.Host) { h.Alias = "web-renamed" })
	})
	b.sync(t)
	a.sync(t)
	if h := a.host(t, hid); h.Alias != "web-renamed" || h.ProxyID == nil || *h.ProxyID != pid || len(a.eng.State.Holds) != 0 {
		t.Fatalf("host on A = %+v, holds %+v", h, a.eng.State.Holds)
	}
	if h := b.host(t, hid); h.ProxyID == nil || *h.ProxyID != store.HeldProxyID {
		t.Fatalf("host on B = %+v", h)
	}
	if len(b.eng.State.Holds) != 1 || b.eng.State.Holds[0].ProxyID == nil || *b.eng.State.Holds[0].ProxyID != pid {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
}

// A synced switch of a PostgreSQL host from verified TLS to "none" waits
// for confirmation, directly and behind a jump host alike (the hop from the
// bastion is cleartext). Once accepted, the saved password, sealed to
// "verify", asks again (KTD25).
func TestSyncedUnencryptedDatabaseHostWaitsForConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	bastion := a.addHost(t, "bastion", "")
	pg := "postgres"
	var direct, jumped string
	a.edit(t, func(d *store.Data) error {
		for _, h := range []store.Host{
			{Alias: "db", Hostname: "db.example", Port: 5432, User: "app", Kind: &pg},
			{Alias: "inner", Hostname: "db.internal", Port: 5432, User: "app", Kind: &pg, JumpHostID: &bastion},
		} {
			h.ID = store.NewID()
			sealed, err := a.vault.Seal([]byte("db-pw"), h.SecretAAD())
			if err != nil {
				return err
			}
			h.Password = sealed
			if h.Jumped() {
				jumped = d.PutHost(h).ID
			} else {
				direct = d.PutHost(h).ID
			}
		}
		return nil
	})
	a.sync(t)
	b.sync(t)
	none := "none"
	a.edit(t, func(d *store.Data) error {
		for _, id := range []string{direct, jumped} {
			if err := d.EditHost(id, func(h *store.Host) { h.TLS = &none }); err != nil {
				return err
			}
		}
		return nil
	})
	a.sync(t)
	b.sync(t)
	b.sync(t)
	for _, id := range []string{direct, jumped} {
		if h := b.host(t, id); h.DatabaseTLS() != "verify" {
			t.Fatalf("synced TLS none applied before confirmation: %+v", h)
		}
	}
	if len(b.eng.State.Holds) != 2 || !b.eng.State.Holds[0].Plaintext || !b.eng.State.Holds[1].Plaintext {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
	if err := b.eng.ResolveHold(direct, true); err != nil {
		t.Fatal(err)
	}
	if h := b.host(t, direct); h.DatabaseTLS() != "none" {
		t.Fatalf("accepting did not apply TLS none: %+v", h)
	}
	if pw, err := b.password(t, direct); err == nil {
		t.Fatalf("the password sealed for verified TLS opened for none: %q", pw)
	}
}

// A synced change of a cluster's trust (its CA, TLS server name or server
// URL) waits for local confirmation, as a weakened setting does (KTD28),
// and is not mistaken for a switch to plaintext.
func TestSyncedClusterTrustChangeWaitsForConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	kind, url, ca := "kubernetes", "https://k8s.example:6443", "-----BEGIN CERTIFICATE-----\nold\n-----END CERTIFICATE-----\n"
	var id string
	a.edit(t, func(d *store.Data) error {
		id = d.PutHost(store.Host{Alias: "prod", Hostname: "k8s.example", Port: 6443, Kind: &kind, URL: &url, CAData: &ca}).ID
		return nil
	})
	a.sync(t)
	b.sync(t)
	if err := b.eng.ResolveHold(id, true); err != nil { // the cluster is new on B
		t.Fatal(err)
	}
	evil, name := "-----BEGIN CERTIFICATE-----\nnew\n-----END CERTIFICATE-----\n", "attacker.example"
	a.edit(t, func(d *store.Data) error {
		return d.EditHost(id, func(h *store.Host) { h.CAData, h.TLSServerName = &evil, &name })
	})
	a.sync(t)
	b.sync(t)
	b.sync(t)
	h := b.host(t, id)
	if store.Deref(h.CAData) != ca || h.TLSServerName != nil || store.Deref(h.URL) != url {
		t.Fatalf("the synced trust applied before confirmation: %+v", h)
	}
	if len(b.eng.State.Holds) != 1 || b.eng.State.Holds[0].Cluster == nil || b.eng.State.Holds[0].Plaintext {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
	if err := b.eng.ResolveHold(id, true); err != nil {
		t.Fatal(err)
	}
	if h := b.host(t, id); store.Deref(h.CAData) != evil || store.Deref(h.TLSServerName) != name || store.Deref(h.URL) != url {
		t.Fatalf("accepting did not apply the synced trust: %+v", h)
	}
}

// A cluster new here that arrives by sync brings its CA and TLS server
// name only after local confirmation, and meanwhile cannot connect at all,
// like a host new here with a proxy (KTD28). Accepting gives it the synced
// trust and its token; refusing leaves it with none of that trust.
func TestSyncedNewClusterWaitsForConfirmation(t *testing.T) {
	for _, accept := range []bool{true, false} {
		remote := &memBackend{}
		a, b := newDevice(t, remote), newDevice(t, remote)
		kind, url, ca, name := "kubernetes", "https://k8s.example:6443", "-----BEGIN CERTIFICATE-----\nforeign\n-----END CERTIFICATE-----\n", "k8s.internal"
		var id string
		a.edit(t, func(d *store.Data) error {
			h := store.Host{Meta: store.Meta{ID: store.NewID()}, Alias: "prod", Hostname: "k8s.example", Port: 6443, Kind: &kind, URL: &url, CAData: &ca, TLSServerName: &name}
			sealed, err := a.vault.Seal([]byte("cluster-token"), h.SecretAAD())
			h.Password = sealed
			id = d.PutHost(h).ID
			return err
		})
		a.sync(t)
		b.sync(t)
		first := append([]Hold(nil), b.eng.State.Holds...)
		b.sync(t)
		h := b.host(t, id)
		if h.CAData != nil || h.TLSServerName != nil || store.Deref(h.URL) != url || h.ProxyID == nil || *h.ProxyID != store.HeldProxyID {
			t.Fatalf("the new cluster on B before confirmation: %+v", h)
		}
		want := ClusterTrust{URL: url, Hostname: "k8s.example", Port: 6443, CAData: ca, TLSServerName: name}
		if hs := b.eng.State.Holds; len(hs) != 1 || hs[0].Cluster == nil || *hs[0].Cluster != want || !reflect.DeepEqual(hs, first) {
			t.Fatalf("holds = %+v, first pass %+v", hs, first)
		}
		a.sync(t)
		if h := a.host(t, id); store.Deref(h.CAData) != ca || h.Proxied() {
			t.Fatalf("B's hold changed the cluster on A: %+v", h)
		}
		if err := b.eng.ResolveHold(id, accept); err != nil {
			t.Fatal(err)
		}
		h = b.host(t, id)
		if h.Proxied() || len(b.eng.State.Holds) != 0 {
			t.Fatalf("accept %v: %+v, holds %+v", accept, h, b.eng.State.Holds)
		}
		if accept {
			if pw, err := b.password(t, id); store.Deref(h.CAData) != ca || store.Deref(h.TLSServerName) != name || err != nil || pw != "cluster-token" {
				t.Fatalf("accepting: %+v, token %q, %v", h, pw, err)
			}
		} else if h.CAData != nil || h.TLSServerName != nil {
			t.Fatalf("refusing kept the synced trust: %+v", h)
		}
	}
}

// An Elasticsearch host is addressed by URL: a synced switch from https://
// to http:// waits for confirmation and keeps the https:// URL meanwhile,
// rather than gaining a TLS mode it does not use (KTD25).
func TestSyncedHTTPElasticsearchHostWaitsForConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	es, secure := "elasticsearch", "https://es.example:9200"
	var id string
	a.edit(t, func(d *store.Data) error {
		h := store.Host{Alias: "es", Hostname: "es.example", Port: 9200, User: "elastic", Kind: &es, URL: &secure}
		h.ID = store.NewID()
		id = d.PutHost(h).ID
		return nil
	})
	a.sync(t)
	b.sync(t)
	plain := "http://es.example:9200"
	a.edit(t, func(d *store.Data) error { return d.EditHost(id, func(h *store.Host) { h.URL = &plain }) })
	a.sync(t)
	b.sync(t)
	b.sync(t)
	if h := b.host(t, id); store.Deref(h.URL) != secure || h.TLS != nil {
		t.Fatalf("synced http:// applied before confirmation: %+v", h)
	}
	if len(b.eng.State.Holds) != 1 || !b.eng.State.Holds[0].Plaintext {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
	if err := b.eng.ResolveHold(id, true); err != nil {
		t.Fatal(err)
	}
	if h := b.host(t, id); store.Deref(h.URL) != plain || h.TLS != nil {
		t.Fatalf("accepting did not apply the http:// URL: %+v", h)
	}
}

// An rqlite host connects to its URL: a synced switch from https:// to
// http:// waits for confirmation with the https:// URL kept, not a TLS
// mode it does not read.
func TestSyncedHTTPRqliteHostWaitsForConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	rq, secure := "rqlite", "https://rq.example:4001"
	var id string
	a.edit(t, func(d *store.Data) error {
		id = d.PutHost(store.Host{Meta: store.Meta{ID: store.NewID()}, Alias: "rq", Kind: &rq, URL: &secure}).ID
		return nil
	})
	a.sync(t)
	b.sync(t)
	plain := "http://rq.example:4001"
	a.edit(t, func(d *store.Data) error { return d.EditHost(id, func(h *store.Host) { h.URL = &plain }) })
	a.sync(t)
	b.sync(t)
	b.sync(t)
	if h := b.host(t, id); store.Deref(h.URL) != secure || h.TLS != nil {
		t.Fatalf("held host = %+v", h)
	}
	if err := b.eng.ResolveHold(id, true); err != nil {
		t.Fatal(err)
	}
	if h := b.host(t, id); store.Deref(h.URL) != plain || h.TLS != nil {
		t.Fatalf("accepted host = %+v", h)
	}
}

// setGroup saves a settings group on d as a local change, keeping its other
// fields.
func (d *device) setGroup(t *testing.T, id string, fn func(g *store.SettingsGroup)) {
	t.Helper()
	d.edit(t, func(data *store.Data) error {
		g := store.SettingsGroup{Meta: store.Meta{ID: id}}
		if cur := data.SettingsGroup(id); cur != nil {
			g = *cur
		}
		fn(&g)
		data.PutSettings(g)
		return nil
	})
}

// Each settings group merges on its own, so changes to different groups
// on two computers both survive (KTD37).
func TestSettingsGroupsSyncOnTheirOwn(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	a.setGroup(t, store.GroupAppearance, func(g *store.SettingsGroup) { g.Theme, g.FontSize = "light", 15 })
	b.setGroup(t, store.GroupLanguage, func(g *store.SettingsGroup) { g.Language = "de" })
	a.sync(t)
	b.sync(t) // settings alone do not make B ask "use the target's data or merge"
	a.sync(t)
	for name, d := range map[string]*device{"A": a, "B": b} {
		data := d.data(t)
		ap, lang := data.SettingsGroup(store.GroupAppearance), data.SettingsGroup(store.GroupLanguage)
		if ap == nil || ap.Theme != "light" || ap.FontSize != 15 || lang == nil || lang.Language != "de" {
			t.Fatalf("settings on %s = %+v", name, data.Settings.Items)
		}
	}
}

// A synced AI execution mode weaker than this computer's waits for local
// confirmation; a stricter one applies at once (KTD13, KTD33).
func TestSyncedWeakerAIModeWaitsForConfirmation(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	a.addHost(t, "web", "")
	a.sync(t)
	b.sync(t)
	a.setGroup(t, store.GroupAI, func(g *store.SettingsGroup) { g.ExecutionMode = store.AIDangerousOnly })
	a.sync(t)
	b.sync(t)
	b.sync(t) // a second pass must not apply it either
	if m := b.data(t).AIMode(); m != store.AIConfirmAll {
		t.Fatalf("mode on B = %q before confirmation", m)
	}
	if len(b.eng.State.Holds) != 1 || b.eng.State.Holds[0].Kind != "aimode" || b.eng.State.Holds[0].Mode != store.AIDangerousOnly {
		t.Fatalf("holds = %+v", b.eng.State.Holds)
	}
	a.sync(t)
	if m := a.data(t).AIMode(); m != store.AIDangerousOnly {
		t.Fatalf("B's hold changed the mode on A to %q", m)
	}
	if err := b.eng.ResolveHold(store.GroupAI, true); err != nil {
		t.Fatal(err)
	}
	if m := b.data(t).AIMode(); m != store.AIDangerousOnly || len(b.eng.State.Holds) != 0 {
		t.Fatalf("after accepting: mode %q, holds %+v", m, b.eng.State.Holds)
	}

	a.setGroup(t, store.GroupAI, func(g *store.SettingsGroup) { g.ExecutionMode = store.AIConfirmAll })
	a.sync(t)
	b.sync(t)
	if m := b.data(t).AIMode(); m != store.AIConfirmAll || len(b.eng.State.Holds) != 0 {
		t.Fatalf("a stricter synced mode: %q, holds %+v", m, b.eng.State.Holds)
	}
}

// Refusing a held mode keeps this computer's and sends it to the others.
func TestRefusedAIModeHoldKeepsTheLocalMode(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	a.setGroup(t, store.GroupAI, func(g *store.SettingsGroup) { g.ExecutionMode = store.AIDangerousWrite })
	a.sync(t)
	b.sync(t)
	if err := b.eng.ResolveHold(store.GroupAI, false); err != nil {
		t.Fatal(err)
	}
	b.sync(t)
	a.sync(t)
	if ma, mb := a.data(t).AIMode(), b.data(t).AIMode(); ma != store.AIConfirmAll || mb != store.AIConfirmAll || len(b.eng.State.Holds) != 0 {
		t.Fatalf("after refusing: A %q, B %q, holds %+v", ma, mb, b.eng.State.Holds)
	}
}

// Restoring a lost version of the ai group would set the mode here with no
// confirmation; the mode changes only through its own verified path.
func TestLostAIModeVersionIsNotRestored(t *testing.T) {
	b := newDevice(t, &memBackend{})
	b.eng.State.Conflicts = []Conflict{{ID: "c1", Collection: "settings", ItemID: store.GroupAI, Kind: "edit",
		Raw: json.RawMessage(`{"id":"ai","revision":3,"executionMode":"dangerous-only"}`)}}
	if err := b.eng.RestoreConflict("c1"); err == nil {
		t.Fatal("restored a lost AI mode")
	}
	if m := b.data(t).AIMode(); m != store.AIConfirmAll {
		t.Fatalf("mode = %q", m)
	}
}

// A saved workspace syncs with its host IDs and layout only: no secret or
// hostname of its hosts travels in it (KTD40).
func TestWorkspacesSyncWithHostIDsOnly(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	web, db := a.addHost(t, "web", "web secret"), a.addHost(t, "db", "")
	var id string
	a.edit(t, func(d *store.Data) error {
		id = d.PutWorkspace(store.Workspace{Name: "prod", Layout: store.WorkspaceNode{
			Dir: "row", Sizes: []float64{0.5, 0.5}, Children: []store.WorkspaceNode{{HostID: web}, {HostID: db}},
		}}).ID
		return nil
	})
	a.sync(t)
	b.sync(t)
	w := b.data(t).Workspace(id)
	if w == nil || w.Name != "prod" || w.Layout.Children[0].HostID != web || w.Layout.Children[1].HostID != db {
		t.Fatalf("synced workspace = %+v", w)
	}
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"web secret", "web.example", "db.example", "root", b.host(t, web).Password} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Fatalf("synced workspace carries %q: %s", leak, raw)
		}
	}
}

// Skills and templates sync whole, the agent-authored mark included; a
// body that looks like markup is carried as text.
func TestSkillsSync(t *testing.T) {
	remote := &memBackend{}
	a, b := newDevice(t, remote), newDevice(t, remote)
	var id string
	a.edit(t, func(d *store.Data) error {
		id = d.PutSkill(store.Skill{Name: "deploy-check", Kind: store.SkillKind, Body: "Check <script>alert(1)</script>", AgentAuthored: true}).ID
		return nil
	})
	a.sync(t)
	b.sync(t)
	s := b.data(t).Skill(id)
	if s == nil || s.Name != "deploy-check" || s.Body != "Check <script>alert(1)</script>" || !s.AgentAuthored {
		t.Fatalf("synced skill = %+v", s)
	}
}
