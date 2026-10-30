package sync

import (
	"errors"
	"reflect"
	"slices"
	"strings"

	"hostbay/internal/sshx"
	"hostbay/internal/store"
)

// Synced security settings apply on a computer only after a local
// confirmation (KTD13). Today that is a tunnel set to listen beyond
// loopback (R33); a host set to plain FTP, an http:// URL or a database TLS
// mode of "none" (KTD25), to forward the
// agent or X11 (KTD20), to offer insecure algorithms or to use another proxy
// or VPN; a proxy pointed at another address (KTD22); a VPN profile pointed
// at another gateway or switched to another type or tier (KTD4); and a Kubernetes host pointed at
// another server or trusting another CA, or new here with a CA or TLS
// server name of its own (KTD28); an AI execution mode weaker than this
// computer's (KTD33); and a post-login rule created or changed on another
// computer (KTD39). MCP settings never sync (KTD37).

// Hold kinds this build resolves. Any other kind came from a newer Hostbay
// and stays held and listed, untouched.
var holdKinds = []string{"", "host", "proxy", "vpn", "aimode", "postlogin"} // "" is a tunnel

// unknownHolds are the holds of a kind this build does not know.
func unknownHolds(holds []Hold) []Hold {
	var out []Hold
	for _, h := range holds {
		if !slices.Contains(holdKinds, h.Kind) {
			out = append(out, h)
		}
	}
	return out
}

// guarded is what this computer has confirmed before a merge: the settings
// a synced change may only weaken after confirmation.
type guarded struct {
	tunnels map[string]bool  // allowed beyond loopback
	hosts   map[string]guard // by host ID
	proxies map[string]Endpoint
	vpns    map[string]Endpoint
	aiMode  string                // the mode in force
	rules   map[string]store.Meta // post-login rules allowed to run, as confirmed
}

// guard is one host's guarded settings.
type guard struct {
	plain bool
	agent bool
	x11   bool
	algos *store.Algorithms
	proxy string
	vpn   string
	// cluster is a Kubernetes host's server and trust anchor, else nil.
	cluster *ClusterTrust
}

func guardOf(h *store.Host) guard {
	g := guard{plain: h.Plaintext(), agent: h.Forwards(), x11: h.ForwardsX11(), algos: h.Algorithms, cluster: clusterOf(h)}
	if h.ProxyID != nil {
		g.proxy = *h.ProxyID
	}
	if h.VPNID != nil {
		g.vpn = *h.VPNID
	}
	return g
}

// clusterOf is a Kubernetes host's trust, or nil for any other kind.
func clusterOf(h *store.Host) *ClusterTrust {
	if h.KindName() != "kubernetes" {
		return nil
	}
	return &ClusterTrust{URL: store.Deref(h.URL), Hostname: h.Hostname, Port: h.Port, CAData: store.Deref(h.CAData), TLSServerName: store.Deref(h.TLSServerName)}
}

// setCluster points h at c.
func setCluster(h *store.Host, c ClusterTrust) {
	opt := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	h.URL, h.Hostname, h.Port, h.CAData, h.TLSServerName = opt(c.URL), c.Hostname, c.Port, opt(c.CAData), opt(c.TLSServerName)
}

func endpointOf(p *store.Proxy) Endpoint { return Endpoint{Type: p.Type, Host: p.Host, Port: p.Port} }

func vpnEndpointOf(v *store.VPN) Endpoint {
	return Endpoint{Type: v.Type, Host: v.Gateway, Port: v.Port, Tier: v.Tier}
}

// setVPNEndpoint points v at e.
func setVPNEndpoint(v *store.VPN, e Endpoint) {
	v.Type, v.Gateway, v.Port, v.Tier = e.Type, e.Host, e.Port, e.Tier
}

// confirmed records d's guarded settings before the merge.
func confirmed(d *store.Data) guarded {
	g := guarded{tunnels: map[string]bool{}, hosts: map[string]guard{}, proxies: map[string]Endpoint{}, vpns: map[string]Endpoint{}, aiMode: d.AIMode(), rules: map[string]store.Meta{}}
	for _, r := range d.PostLogin.Items {
		if !r.Held {
			g.rules[r.ID] = r.Meta
		}
	}
	for _, t := range d.Tunnels.Items {
		g.tunnels[t.ID] = t.AllowExternal
	}
	for i := range d.Hosts.Items {
		g.hosts[d.Hosts.Items[i].ID] = guardOf(&d.Hosts.Items[i])
	}
	for i := range d.Proxies.Items {
		g.proxies[d.Proxies.Items[i].ID] = endpointOf(&d.Proxies.Items[i])
	}
	for i := range d.VPNs.Items {
		g.vpns[d.VPNs.Items[i].ID] = vpnEndpointOf(&d.VPNs.Items[i])
	}
	return g
}

// liftHolds shows held settings as synced, so the merge and the upload see
// the real setting rather than this computer's stand-in.
func (e *Engine) liftHolds(d *store.Data) {
	synced := map[string]*string{}    // host ID: the synced proxy a hold waits on
	syncedVPN := map[string]*string{} // host ID: the synced VPN a hold waits on
	for _, h := range e.State.Holds {
		switch h.Kind {
		case "host":
			if h.ProxyID != nil && *h.ProxyID != "" {
				synced[h.ID] = new(*h.ProxyID)
			}
			if h.VPNID != nil && *h.VPNID != "" {
				syncedVPN[h.ID] = new(*h.VPNID)
			}
			if host := d.Host(h.ID); host != nil && sameMeta(host.Meta, h.Meta) {
				h.applyHost(host)
			}
		case "proxy":
			if p := d.Proxy(h.ID); p != nil && sameMeta(p.Meta, h.Meta) && h.Endpoint != nil {
				p.Type, p.Host, p.Port = h.Endpoint.Type, h.Endpoint.Host, h.Endpoint.Port
			}
		case "vpn":
			if v := d.VPN(h.ID); v != nil && sameMeta(v.Meta, h.Meta) && h.Endpoint != nil {
				setVPNEndpoint(v, *h.Endpoint)
			}
		case "aimode":
			if g := d.SettingsGroup(h.ID); g != nil && sameMeta(g.Meta, h.Meta) {
				g.ExecutionMode = h.Mode
			}
		case "":
			if t := d.Tunnel(h.ID); t != nil && sameMeta(t.Meta, h.Meta) {
				t.AllowExternal = true
			}
		}
	}
	// Held is this computer's stand-in and must not travel; holdNew holds
	// every rule not confirmed here again.
	for i := range d.PostLogin.Items {
		d.PostLogin.Items[i].Held = false
	}
	// HeldProxyID and HeldVPNID must not travel. A host edited here since
	// its hold still has them: show the synced proxy or VPN, or none, and
	// holdNew holds it again.
	for i := range d.Hosts.Items {
		h := &d.Hosts.Items[i]
		if h.ProxyID != nil && *h.ProxyID == store.HeldProxyID {
			h.ProxyID = synced[h.ID]
		}
		if h.VPNID != nil && *h.VPNID == store.HeldVPNID {
			h.VPNID = syncedVPN[h.ID]
		}
	}
}

// plain reports a host hold on a plaintext setting. A hold saved before
// holds named their settings was only ever that.
func (h Hold) plain() bool {
	return h.Plaintext || (!h.ForwardAgent && !h.X11 && h.Algorithms == nil && h.ProxyID == nil && h.VPNID == nil && h.Cluster == nil)
}

// applyHost sets host to the synced values h waits on.
func (h Hold) applyHost(host *store.Host) {
	if h.plain() {
		setPlaintext(host, true)
	}
	if h.ForwardAgent {
		on := true
		host.ForwardAgent = &on
	}
	if h.X11 {
		on := true
		host.X11 = &on
	}
	if h.Algorithms != nil {
		host.Algorithms = h.Algorithms
	}
	if h.Cluster != nil {
		setCluster(host, *h.Cluster)
	}
	if h.ProxyID != nil {
		host.ProxyID = nil
		if *h.ProxyID != "" {
			host.ProxyID = new(*h.ProxyID)
		}
	}
	if h.VPNID != nil {
		host.VPNID = nil
		if *h.VPNID != "" {
			host.VPNID = new(*h.VPNID)
		}
	}
}

// holdNew puts back what this computer confirmed wherever only another
// computer weakened it, and returns the holds: a tunnel stays on loopback,
// a file host encrypted, a cluster on its confirmed server and CA, a host forwards no agent or X11 and offers its confirmed
// algorithms, and a proxy or VPN profile keeps where it points. A host
// whose proxy or VPN changed keeps its confirmed one; a host new here gets
// HeldProxyID or HeldVPNID, so it cannot connect at all rather than skip
// the proxy or VPN, and so does a cluster new here that brings its own CA
// or TLS server name.
func holdNew(d *store.Data, was guarded) []Hold {
	var holds []Hold
	for i := range d.Tunnels.Items {
		t := &d.Tunnels.Items[i]
		if t.AllowExternal && !was.tunnels[t.ID] {
			t.AllowExternal = false
			holds = append(holds, Hold{ID: t.ID, Name: t.Name, Meta: t.Meta})
		}
	}
	for i := range d.Hosts.Items {
		h := &d.Hosts.Items[i]
		g, known := was.hosts[h.ID]
		hd := Hold{ID: h.ID, Kind: "host", Name: h.Alias, Meta: h.Meta}
		held := false
		if h.Plaintext() && !g.plain {
			setPlaintext(h, false)
			hd.Plaintext, held = true, true
		}
		if h.Forwards() && !g.agent {
			h.ForwardAgent = nil
			hd.ForwardAgent, held = true, true
		}
		if h.ForwardsX11() && !g.x11 {
			h.X11 = nil
			hd.X11, held = true, true
		}
		if h.Algorithms != nil && sshx.Algorithms(*h.Algorithms).Insecure() && !reflect.DeepEqual(h.Algorithms, g.algos) {
			hd.Algorithms, h.Algorithms, held = h.Algorithms, g.algos, true
		}
		// A changed cluster keeps what this computer confirmed. One new here
		// keeps its server but none of its own CA or server name, and cannot
		// connect at all until confirmed, as a host new here with a proxy.
		fresh := false
		if c := clusterOf(h); c != nil {
			keep := g.cluster
			if keep == nil {
				keep = &ClusterTrust{URL: c.URL, Hostname: c.Hostname, Port: c.Port}
			}
			if *c != *keep {
				hd.Cluster, held, fresh = c, true, g.cluster == nil
				setCluster(h, *keep)
			}
		}
		if p := guardOf(h).proxy; p != g.proxy || fresh {
			hd.ProxyID, h.ProxyID, held = new(p), nil, true
			switch {
			case !known || fresh:
				h.ProxyID = new(store.HeldProxyID)
			case g.proxy != "":
				h.ProxyID = new(g.proxy)
			}
		}
		if v := guardOf(h).vpn; v != g.vpn {
			hd.VPNID, h.VPNID, held = new(v), nil, true
			switch {
			case !known:
				h.VPNID = new(store.HeldVPNID)
			case g.vpn != "":
				h.VPNID = new(g.vpn)
			}
		}
		if held {
			holds = append(holds, hd)
		}
	}
	for i := range d.Proxies.Items {
		p := &d.Proxies.Items[i]
		if old, ok := was.proxies[p.ID]; ok && endpointOf(p) != old {
			synced := endpointOf(p)
			p.Type, p.Host, p.Port = old.Type, old.Host, old.Port
			holds = append(holds, Hold{ID: p.ID, Kind: "proxy", Name: p.Name, Meta: p.Meta, Endpoint: &synced})
		}
	}
	for i := range d.VPNs.Items {
		v := &d.VPNs.Items[i]
		if old, ok := was.vpns[v.ID]; ok && vpnEndpointOf(v) != old {
			synced := vpnEndpointOf(v)
			setVPNEndpoint(v, old)
			holds = append(holds, Hold{ID: v.ID, Kind: "vpn", Name: v.Name, Meta: v.Meta, Endpoint: &synced})
		}
	}
	// A rule runs only as this computer confirmed it: one new here, or
	// changed since, waits.
	for i := range d.PostLogin.Items {
		r := &d.PostLogin.Items[i]
		if m, ok := was.rules[r.ID]; ok && sameMeta(m, r.Meta) {
			continue
		}
		r.Held = true
		name := "Post-login rule"
		if h := d.Host(r.HostID); h != nil {
			name += " for " + h.Alias
		}
		holds = append(holds, Hold{ID: r.ID, Kind: "postlogin", Name: name, Meta: r.Meta})
	}
	// A stricter synced mode applies at once; a weaker one waits.
	if g := d.SettingsGroup(store.GroupAI); g != nil && store.AIModeRank(g.ExecutionMode) > store.AIModeRank(was.aiMode) {
		holds = append(holds, Hold{ID: g.ID, Kind: "aimode", Name: "AI execution mode", Meta: g.Meta, Mode: g.ExecutionMode})
		g.ExecutionMode = was.aiMode
	}
	return holds
}

// setPlaintext switches a host between its plaintext setting and the
// encrypted stand-in: plain FTP and explicit FTPS, plain VNC or SPICE and
// their VeNCrypt or TLS default, a database host's "none" and "verify", or
// an http:// and an https:// URL. Its sealed password is bound to the
// setting, so it opens only under the synced one. A VNC or SPICE host made
// plain by losing its jump host keeps its jump host removed but goes back
// to the encrypted default.
func setPlaintext(h *store.Host, plain bool) {
	// Elasticsearch and rqlite hosts are databases addressed by URL, like WebDAV.
	k := h.KindName()
	byTLS := k == "vnc" || k == "spice" || (h.IsDatabase() && k != "elasticsearch" && k != "rqlite")
	switch {
	case k == "ftp" && plain:
		h.TLS = new("none")
	case k == "ftp":
		h.TLS = new("explicit")
	case byTLS && plain:
		h.TLS = new("none")
	case byTLS:
		h.TLS = nil
	case h.URL != nil && plain:
		h.URL = new("http://" + strings.TrimPrefix(*h.URL, "https://"))
	case h.URL != nil:
		h.URL = new("https://" + strings.TrimPrefix(*h.URL, "http://"))
	}
}

// ResolveHold applies (accept) or refuses a held setting. Refusing is a
// local change, so it reaches the other computers. A host new here that
// refuses its synced proxy or VPN is left with none: HeldProxyID and
// HeldVPNID are this computer's stand-ins and must not travel.
func (e *Engine) ResolveHold(id string, accept bool) error {
	i := -1
	for j, h := range e.State.Holds {
		if h.ID == id {
			i = j
		}
	}
	if i < 0 {
		return errors.New("nothing is waiting for confirmation with that ID")
	}
	h := e.State.Holds[i]
	if !slices.Contains(holdKinds, h.Kind) {
		return errors.New("a newer Hostbay set this aside; update Hostbay to allow or refuse it")
	}
	err := e.Store.Edit(func(d *store.Data) error {
		var meta *store.Meta
		var apply, refuse func()
		switch h.Kind {
		case "host":
			host := d.Host(id)
			if host == nil {
				return nil
			}
			meta = &host.Meta
			apply = func() { h.applyHost(host) }
			refuse = func() {
				if host.ProxyID != nil && *host.ProxyID == store.HeldProxyID {
					host.ProxyID = nil
				}
				if host.VPNID != nil && *host.VPNID == store.HeldVPNID {
					host.VPNID = nil
				}
			}
		case "proxy":
			p := d.Proxy(id)
			if p == nil || h.Endpoint == nil {
				return nil
			}
			meta = &p.Meta
			apply = func() { p.Type, p.Host, p.Port = h.Endpoint.Type, h.Endpoint.Host, h.Endpoint.Port }
		case "vpn":
			v := d.VPN(id)
			if v == nil || h.Endpoint == nil {
				return nil
			}
			meta = &v.Meta
			apply = func() { setVPNEndpoint(v, *h.Endpoint) }
		case "aimode":
			g := d.SettingsGroup(id)
			if g == nil {
				return nil
			}
			meta = &g.Meta
			apply = func() { g.ExecutionMode = h.Mode }
		case "postlogin":
			// Refusing removes the rule: there is no confirmed version to keep.
			r := d.PostLoginRule(id)
			if r == nil || !sameMeta(r.Meta, h.Meta) {
				return nil
			}
			if accept {
				r.Held = false
				return nil
			}
			return d.PurgePostLogin(id)
		case "":
			t := d.Tunnel(id)
			if t == nil {
				return nil
			}
			meta = &t.Meta
			apply = func() { t.AllowExternal = true }
		}
		if !sameMeta(*meta, h.Meta) {
			return nil // changed since; the next sync decides again
		}
		if accept {
			apply() // the synced value: no new revision
			return nil
		}
		if refuse != nil {
			refuse()
		}
		e.Store.Touch(meta)
		return nil
	})
	if err != nil {
		return err
	}
	e.State.Holds = append(e.State.Holds[:i], e.State.Holds[i+1:]...)
	return SaveState(e.Store, e.State)
}
