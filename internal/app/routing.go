package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/netd"
	"hostbay/internal/store"
	"hostbay/internal/vpn/detect"
	"hostbay/internal/vpn/proto"
	"hostbay/internal/vpn/routing"
)

// RoutingStateEvent carries the RoutingState whenever it changes.
const RoutingStateEvent = "routing-state"

// A profile's routing state after an apply.
const (
	RouteApplied  = "applied"
	RoutePartly   = "partly applied" // at least one of its changes failed (AE5)
	RouteFighting = "fighting"       // its vendor keeps reverting the routes
)

var (
	// routingDebounce gathers a burst of route events into one re-apply;
	// routingCap is the least time between automatic re-applies of one
	// profile, so Hostbay does not fight a vendor agent (KTD9).
	routingDebounce = 300 * time.Millisecond
	routingCap      = 5 * time.Second
	// domainRefresh is how often a connected VPN's listed domains are
	// resolved again; route events read the last answers. domainRetry is
	// how soon a domain that has no answer yet is tried again.
	domainRefresh = 5 * time.Minute
	domainRetry   = 30 * time.Second
)

// RoutingState is what Settings shows about routing.
type RoutingState struct {
	Profiles map[string]string `json:"profiles"` // profile ID: RouteApplied, RoutePartly or RouteFighting
	// Reset is true after Reset until the next Apply: nothing re-applies.
	Reset  bool   `json:"reset"`
	Notice string `json:"notice,omitempty"` // the last automatic re-apply, to tell the user (R29)
}

// ChangeResult is one planned change and its outcome.
type ChangeResult struct {
	routing.Change
	Error string `json:"error,omitempty"`
}

// RoutingReport is the outcome of an apply (R26).
type RoutingReport struct {
	Results []ChangeResult `json:"results"`
	Skipped []routing.Skip `json:"skipped"`
	Partly  []string       `json:"partly"` // profiles with a failed change
}

// routeTable is the part of netd.Table routing reads; changes go through
// the helper.
type routeTable interface {
	Routes() ([]netd.SysRoute, error)
	Interfaces() ([]netd.Iface, error)
}

// RoutingService plans and applies system routes for system-wide and
// external VPNs (R25 to R29) through hostbay-netd.
type RoutingService struct {
	st      *store.Store
	vpns    *VPNService
	netd    *NetdService
	table   routeTable
	mtuOf   func(iface string) int
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	emit    func(name string, data any)
	now     func() time.Time

	kicks  chan struct{}
	stop   context.CancelFunc
	exited chan struct{} // closed when the re-apply loop has ended

	applying sync.Mutex // one apply at a time

	mu     sync.Mutex
	state  RoutingState
	added  []routing.Change     // routes this helper connection added, which Reset removes
	last   map[string]time.Time // profile: its last automatic re-apply
	fights map[string]int       // profile: re-applies in a row, each soon after the last
	// domains is each connected system-wide or external profile's listed
	// domains as last resolved.
	domains map[string]domainCache
}

type domainCache struct {
	at    time.Time
	list  string // the profile's domains when resolved, so an edit resolves again
	names []routing.Domain
}

// NewRoutingService links routing to the helper connection and to VPN
// states, so it re-applies on helper (re)connects, which include startup,
// on route events and on VPNs connecting.
func NewRoutingService(st *store.Store, vpns *VPNService, nd *NetdService) *RoutingService {
	s := &RoutingService{
		st: st, vpns: vpns, netd: nd, table: netd.SystemTable(), mtuOf: interfaceMTU, resolve: resolveGateway, emit: emitEvent, now: time.Now,
		kicks: make(chan struct{}, 1), state: RoutingState{Profiles: map[string]string{}}, last: map[string]time.Time{}, fights: map[string]int{},
		domains: map[string]domainCache{},
	}
	connect := nd.onConnect
	nd.onConnect = func() {
		if connect != nil {
			connect()
		}
		s.mu.Lock()
		s.added = nil // the helper swept the last connection's routes
		s.mu.Unlock()
		s.kick()
	}
	nd.onEvent = s.kick
	vpns.forgotten = func(id string) {
		s.mu.Lock()
		delete(s.state.Profiles, id)
		delete(s.last, id)
		delete(s.fights, id)
		s.mu.Unlock()
	}
	vpns.checkConnect = s.conflict
	vpns.tunnels = func() tunnelClient {
		c, _ := nd.client().(tunnelClient)
		return c
	}
	// VPNService emits with its lock held, so this only signals.
	emit := vpns.emit
	vpns.emit = func(name string, data any) {
		emit(name, data)
		// A disconnect too, so the profile's last routing status goes.
		if _, ok := data.(VPNState); ok && name == VPNStateEvent {
			s.kick()
		}
	}
	return s
}

func interfaceMTU(name string) int {
	if i, err := net.InterfaceByName(name); err == nil {
		return i.MTU
	}
	return 0
}

// resolveGateway is every address of a VPN gateway, so a dual-stack
// gateway is pinned in each family.
// shortcut: resolves the name again rather than asking the helper which
// address it dialed; upgrade if a gateway's DNS answers vary between calls.
func resolveGateway(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func (s *RoutingService) ServiceStartup(context.Context, application.ServiceOptions) error {
	ctx, stop := context.WithCancel(context.Background())
	s.stop, s.exited = stop, make(chan struct{})
	go func() {
		defer close(s.exited)
		s.loop(ctx)
	}()
	return nil
}

func (s *RoutingService) ServiceShutdown() error {
	if s.stop != nil {
		s.stop()
		<-s.exited
	}
	return nil
}

func (s *RoutingService) kick() {
	select {
	case s.kicks <- struct{}{}:
	default:
	}
}

// loop re-applies once per burst of kicks, and again when a profile held
// back by the cap may run.
func (s *RoutingService) loop(ctx context.Context) {
	var wake <-chan time.Time
	refresh := time.NewTicker(domainRefresh)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kicks:
		case <-wake:
		case <-refresh.C:
			// Looked up again on the tick itself: a cache stamped since the
			// last tick would still count as fresh.
			s.mu.Lock()
			n := len(s.domains)
			s.mu.Unlock()
			if n == 0 {
				continue
			}
			s.resolveDomains(ctx, true)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(routingDebounce):
		}
		select {
		case <-s.kicks:
		default:
		}
		wake = nil
		if d := s.reapply(ctx); d > 0 {
			wake = time.After(d)
		}
	}
}

// reapply applies what differs from the plan for profiles with auto-apply
// on (R29), each at most once per routingCap, and every gateway pin; it
// returns how long until a held-back profile may run.
func (s *RoutingService) reapply(ctx context.Context) time.Duration {
	connected := map[string]bool{}
	for _, st := range s.vpns.VPNStates() {
		connected[st.ID] = st.Status == VPNConnected
	}
	s.mu.Lock()
	// A status is about this connection's routes; the next one starts over.
	n := len(s.state.Profiles)
	maps.DeleteFunc(s.state.Profiles, func(id, _ string) bool { return id != "" && !connected[id] })
	if len(s.state.Profiles) != n {
		s.announce()
	}
	reset := s.state.Reset
	s.mu.Unlock()
	if reset || s.netd.client() == nil {
		return 0
	}
	s.resolveDomains(ctx, false)
	d, err := s.st.Data()
	if err != nil {
		return 0
	}
	auto, anyAuto := map[string]bool{}, false
	for _, v := range d.VPNs.Items {
		auto[v.ID] = v.AutoApplies()
		anyAuto = anyAuto || auto[v.ID]
	}
	s.applying.Lock()
	defer s.applying.Unlock()
	plan, err := s.plan(ctx, "", "")
	if err != nil {
		return 0
	}
	now, held, run := s.now(), time.Duration(0), map[string]bool{}
	s.mu.Lock()
	for _, c := range plan.Changes {
		// Pins run whatever auto-apply says and are never held back, so an
		// app-scoped tunnel keeps its gateway (KTD9).
		key := c.Profile
		if c.Kind == routing.KindPin || run[key] || !auto[key] && !(key == "" && anyAuto) {
			continue
		}
		last, ok := s.last[key]
		if since := now.Sub(last); ok && since < routingCap {
			if wait := routingCap - since; held == 0 || wait < held {
				held = wait
			}
			continue
		}
		if ok && now.Sub(last) < 3*routingCap {
			s.fights[key]++
		} else {
			s.fights[key] = 0
		}
		s.last[key], run[key] = now, true
	}
	s.mu.Unlock()
	var changes []routing.Change
	for _, c := range plan.Changes {
		if c.Kind == routing.KindPin || run[c.Profile] {
			changes = append(changes, c)
		}
	}
	if len(changes) > 0 {
		rep := s.execute(ctx, changes, plan.Skipped)
		s.mu.Lock()
		for key := range run {
			if s.fights[key] >= 2 && key != "" {
				s.state.Profiles[key] = RouteFighting
			}
		}
		s.state.Notice = fmt.Sprintf("Hostbay re-applied %d route changes at %s", len(rep.Results), now.Format("15:04:05"))
		s.announce()
		s.mu.Unlock()
	}
	return held
}

// plan builds the plan from the saved profiles, with profile id's mode
// replaced by mode when id is set.
func (s *RoutingService) plan(ctx context.Context, id, mode string) (routing.Plan, error) {
	if id != "" && mode != routing.PerDestination && mode != routing.Full && mode != routing.Split {
		return routing.Plan{}, fmt.Errorf("unknown routing mode %q", mode)
	}
	d, err := s.st.Data()
	if err != nil {
		return routing.Plan{}, err
	}
	l, err := s.st.Local()
	if err != nil {
		return routing.Plan{}, err
	}
	_, labels, err := s.vpns.labelNow(ctx, d, l)
	if err != nil {
		return routing.Plan{}, err
	}
	routes, err := s.table.Routes()
	if err != nil {
		return routing.Plan{}, fmt.Errorf("reading the route table: %w", err)
	}
	ifaces, err := s.table.Interfaces()
	if err != nil {
		return routing.Plan{}, fmt.Errorf("listing interfaces: %w", err)
	}
	in := routing.Input{Labels: labels, Local: l.RoutingLocal, Routes: routes, Tunnels: map[string]bool{}, MTUs: map[string]int{}}
	s.mu.Lock()
	for _, c := range s.added {
		if c.Domain != "" {
			in.Added = append(in.Added, c)
		}
	}
	domains := maps.Clone(s.domains)
	s.mu.Unlock()
	for _, i := range ifaces {
		if i.Tunnel {
			in.Tunnels[i.Name], in.MTUs[i.Name] = true, s.mtuOf(i.Name)
		}
	}
	for _, v := range d.VPNs.Items {
		p := profileFor(v, l)
		if v.ID == id {
			p.Mode = mode
		}
		if c, ok := domains[v.ID]; ok {
			p.Domains = c.names
		} else {
			for _, n := range v.Domains {
				p.Domains = append(p.Domains, routing.Domain{Name: n})
			}
		}
		in.Profiles = append(in.Profiles, p)
	}
	for _, st := range s.vpns.VPNStates() {
		v := d.VPN(st.ID)
		if st.Status != VPNConnected || v == nil {
			continue
		}
		if vpnTier(*v) == store.VPNTierSystem && st.Iface != "" {
			in.Labels = append(in.Labels, detect.Labeled{Tunnel: detect.Tunnel{Iface: st.Iface}, Profile: v.ID})
		}
		// An app-scoped profile's gateway, and a system-wide WireGuard peer, are
		// reached through the physical network; behind another full tunnel their
		// packets would loop into it. (OpenVPN keeps its own server off the
		// tunnel with a route of its own.)
		app := vpnTier(*v) == store.VPNTierApp && v.Type != proto.TypeTailscale
		wg := vpnTier(*v) == store.VPNTierSystem && v.Type == proto.TypeWireGuard
		if (app || wg) && v.Gateway != "" {
			host, _, _ := strings.Cut(v.Gateway, "/") // an AnyConnect gateway may carry a /group path
			as, _ := s.resolve(ctx, host)
			for _, a := range as {
				in.Endpoints = append(in.Endpoints, routing.Endpoint{Profile: v.ID, Name: v.Name, Addr: a.Unmap()})
			}
		}
	}
	return routing.Build(in), nil
}

func profileFor(v store.VPN, l store.Local) routing.Profile {
	return routing.Profile{ID: v.ID, Name: v.Name, Tier: vpnTier(v), Mode: v.RoutingMode, Routes: v.Routes, MTU: v.TunnelMTU(), Narrow: l.VPNNarrow[v.ID]}
}

// execute sends each change to the helper in order and records each
// result; one failure does not stop the rest.
func (s *RoutingService) execute(ctx context.Context, changes []routing.Change, skipped []routing.Skip) RoutingReport {
	c := s.netd.client()
	rep := RoutingReport{Results: []ChangeResult{}, Skipped: skipped, Partly: []string{}}
	failed := map[string]bool{}
	for _, ch := range changes {
		err := errors.New("the network helper is not connected")
		if c != nil {
			switch {
			case ch.Kind == routing.KindMTU:
				err = c.SetMTU(ctx, ch.Iface, ch.MTU)
			case ch.Op == netd.Add:
				err = c.Add(ctx, ch.Route())
			default:
				err = c.Delete(ctx, ch.Route())
			}
		}
		r := ChangeResult{Change: ch}
		if err != nil {
			r.Error = err.Error()
			failed[ch.Profile] = true
		} else if ch.Op == netd.Add {
			s.mu.Lock()
			s.added = append(s.added, ch)
			s.mu.Unlock()
		} else if ch.Domain != "" {
			s.mu.Lock()
			s.added = slices.DeleteFunc(s.added, func(a routing.Change) bool { return a.Dst == ch.Dst && a.Iface == ch.Iface })
			s.mu.Unlock()
		}
		rep.Results = append(rep.Results, r)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range changes {
		if ch.Profile == "" {
			continue
		}
		if failed[ch.Profile] {
			s.state.Profiles[ch.Profile] = RoutePartly
			if !slices.Contains(rep.Partly, ch.Profile) {
				rep.Partly = append(rep.Partly, ch.Profile)
			}
		} else {
			s.state.Profiles[ch.Profile] = RouteApplied
		}
	}
	s.announce()
	return rep
}

// snapshot is st with its own copy of Profiles, safe to hand out.
func (st RoutingState) snapshot() RoutingState {
	st.Profiles = maps.Clone(st.Profiles)
	return st
}

// announce emits the state; call it with s.mu held.
func (s *RoutingService) announce() {
	s.emit(RoutingStateEvent, s.state.snapshot())
}

// State is routing's state now.
func (s *RoutingService) State() RoutingState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.snapshot()
}

// Preview is the plan Apply would run (R26), with profile id in mode when
// id is set.
func (s *RoutingService) Preview(ctx context.Context, id, mode string) (routing.Plan, error) {
	s.resolveDomains(ctx, false)
	return s.plan(ctx, id, mode)
}

// resolveDomains looks up the listed domains of each connected system-wide
// or external profile, each profile at most once per domainRefresh unless
// force is set or its list changed. A failed lookup keeps the addresses it
// had, so a DNS hiccup does not pull a site off its VPN. It runs outside
// the apply lock, so a slow answer never holds up Apply or Reset.
func (s *RoutingService) resolveDomains(ctx context.Context, force bool) {
	d, err := s.st.Data()
	if err != nil {
		return
	}
	connected := map[string]bool{}
	for _, st := range s.vpns.VPNStates() {
		connected[st.ID] = st.Status == VPNConnected
	}
	s.mu.Lock()
	old := maps.Clone(s.domains)
	s.mu.Unlock()
	next, retry := map[string]domainCache{}, false
	for _, v := range d.VPNs.Items {
		if vpnTier(v) == store.VPNTierApp || len(v.Domains) == 0 || !connected[v.ID] {
			continue
		}
		list, now := strings.Join(v.Domains, " "), s.now()
		if c, ok := old[v.ID]; ok && !force && c.list == list && now.Sub(c.at) < domainRefresh {
			next[v.ID] = c
			continue
		}
		c := domainCache{at: now, list: list}
		for _, n := range v.Domains {
			dom := routing.Domain{Name: n}
			as, err := s.resolve(ctx, n)
			if err != nil {
				dom.Err = err.Error()
				if i := slices.IndexFunc(old[v.ID].names, func(o routing.Domain) bool { return o.Name == n }); i >= 0 {
					dom.Addrs = old[v.ID].names[i].Addrs
				}
			}
			for _, a := range as {
				dom.Addrs = append(dom.Addrs, a.Unmap())
			}
			if len(dom.Addrs) == 0 {
				// Nothing routes it until it answers, so ask again soon.
				c.at, retry = now.Add(domainRetry-domainRefresh), true
			}
			c.names = append(c.names, dom)
		}
		next[v.ID] = c
	}
	if retry {
		time.AfterFunc(domainRetry, s.kick)
	}
	s.mu.Lock()
	s.domains = next
	s.mu.Unlock()
}

// Apply saves profile id's mode when id is set, then applies the whole
// plan and reports each change.
func (s *RoutingService) Apply(ctx context.Context, id, mode string) (RoutingReport, error) {
	if s.netd.client() == nil {
		return RoutingReport{}, errors.New("the network helper is not connected")
	}
	s.resolveDomains(ctx, false)
	s.applying.Lock()
	defer s.applying.Unlock()
	plan, err := s.plan(ctx, id, mode)
	if err != nil {
		return RoutingReport{}, err
	}
	if id != "" {
		err := s.st.Edit(func(d *store.Data) error {
			v := d.VPN(id)
			if v == nil {
				return errors.New("VPN profile not found")
			}
			if v.RoutingMode != mode {
				v.RoutingMode = mode
				d.PutVPN(*v)
			}
			return nil
		})
		if err != nil {
			return RoutingReport{}, err
		}
	}
	s.mu.Lock()
	s.state.Reset, s.state.Notice = false, ""
	clear(s.fights)
	s.mu.Unlock()
	return s.execute(ctx, plan.Changes, plan.Skipped), nil
}

// Reset removes every route Hostbay added and restores the vendor routes it
// narrowed: the helper sweeps them when the connection ends, and Hostbay
// reconnects. Ending the connection would also end every system-wide
// tunnel, so while one runs Reset deletes each added route through the live
// connection instead, and keeps the narrowing routes, whose vendor routes
// only the sweep can restore. Nothing re-applies until the next Apply.
func (s *RoutingService) Reset(ctx context.Context) (RoutingReport, error) {
	s.applying.Lock()
	defer s.applying.Unlock()
	c := s.netd.client()
	if c == nil {
		return RoutingReport{}, errors.New("the network helper is not connected")
	}
	s.mu.Lock()
	s.state.Reset, s.state.Notice = true, ""
	clear(s.state.Profiles)
	added := slices.Clone(s.added)
	s.announce()
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if s.tunnelsUp() {
		return s.deleteAdded(ctx, c, added), nil
	}
	var dels []routing.Change
	for _, c := range added {
		if c.Kind != routing.KindMTU {
			c.Kind, c.Op, c.Reason = routing.KindDelete, netd.Delete, "added by Hostbay"
			dels = append(dels, c)
		}
	}
	if err := s.netd.reconnect(ctx); err != nil {
		return RoutingReport{}, err
	}
	routes, err := s.table.Routes()
	if err != nil {
		return RoutingReport{}, fmt.Errorf("reading the route table: %w", err)
	}
	rep := RoutingReport{Results: []ChangeResult{}, Skipped: []routing.Skip{}, Partly: []string{}}
	for _, c := range dels {
		r := ChangeResult{Change: c}
		if slices.ContainsFunc(routes, func(x netd.SysRoute) bool { return x.Dst.String() == c.Dst && (c.Gateway || x.Iface == c.Iface) }) {
			r.Error = "still in the route table"
		}
		rep.Results = append(rep.Results, r)
	}
	return rep, nil
}

// tunnelsUp reports whether a system-wide tunnel runs through the helper
// connection: connected, connecting or waiting to reconnect.
func (s *RoutingService) tunnelsUp() bool {
	d, err := s.st.Data()
	if err != nil {
		return true // unsure: keep the connection, and the tunnels
	}
	return slices.ContainsFunc(s.vpns.active(), func(id string) bool {
		v := d.VPN(id)
		return v != nil && vpnTier(*v) == store.VPNTierSystem && v.Type != proto.TypeIPsec
	})
}

// deleteAdded is Reset through the live connection c: each route in added
// is deleted, except a narrowing route, which stays with the vendor route
// it stands in for until the connection ends.
func (s *RoutingService) deleteAdded(ctx context.Context, c netdClient, added []routing.Change) RoutingReport {
	rep := RoutingReport{Results: []ChangeResult{}, Skipped: []routing.Skip{}, Partly: []string{}}
	var kept []routing.Change
	for _, ch := range added {
		switch ch.Kind {
		case routing.KindMTU:
			continue
		case routing.KindNarrow:
			kept = append(kept, ch)
			rep.Skipped = append(rep.Skipped, routing.Skip{Dst: ch.Dst, Profile: ch.Profile,
				Reason: "kept while a system-wide VPN is connected: the vendor route it narrows comes back only once they disconnect; reset again then"})
			continue
		}
		del := ch
		del.Kind, del.Op, del.Reason = routing.KindDelete, netd.Delete, "added by Hostbay"
		r := ChangeResult{Change: del}
		if err := c.Delete(ctx, ch.Route()); err != nil && !routeGone(err) {
			r.Error = err.Error()
			kept = append(kept, ch)
		}
		rep.Results = append(rep.Results, r)
	}
	s.mu.Lock()
	s.added = kept
	s.mu.Unlock()
	return rep
}

// routeGone reports whether err says the route is no longer in the table,
// as one does once its tunnel has ended: deleting it again can only fail, so
// it must not stay on the list of routes to reset.
func routeGone(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "not in table") || strings.Contains(m, "no such process") || strings.Contains(m, "no such route")
}

// Narrow saves the subnets to keep of a greedy route on profile id's
// tunnel (R27); the plan then replaces the route with them whenever the
// vendor pushes it. An empty keep list is refused.
func (s *RoutingService) Narrow(id, greedy string, keep []string) error {
	cs, err := routing.Narrow(greedy, "", keep)
	if err != nil {
		return err
	}
	var kept []string
	for _, c := range cs[1:] {
		kept = append(kept, c.Dst)
	}
	return s.st.EditLocal(func(l *store.Local) error {
		if l.VPNNarrow == nil {
			l.VPNNarrow = map[string]map[string][]string{}
		}
		if l.VPNNarrow[id] == nil {
			l.VPNNarrow[id] = map[string][]string{}
		}
		l.VPNNarrow[id][cs[0].Dst] = kept
		return nil
	})
}

// SetLocalRoutes saves the destinations per-destination routing sends to
// the local network (R25).
func (s *RoutingService) SetLocalRoutes(dsts []string) error {
	for _, d := range dsts {
		if _, err := routing.ParseDst(d); err != nil {
			return fmt.Errorf("%q is not a network", d)
		}
	}
	return s.st.EditLocal(func(l *store.Local) error {
		l.RoutingLocal = dsts
		return nil
	})
}

// LocalRoutes is the destinations sent to the local network.
func (s *RoutingService) LocalRoutes() ([]string, error) {
	l, err := s.st.Local()
	if l.RoutingLocal == nil {
		l.RoutingLocal = []string{}
	}
	return l.RoutingLocal, err
}

// PinInterface pins profile id to a tunnel interface on this computer, for
// when labeling cannot tell two VPNs apart (R24); "" unpins.
func (s *RoutingService) PinInterface(id, iface string) error {
	if iface != "" {
		ifaces, err := s.table.Interfaces()
		if err != nil {
			return err
		}
		if !slices.Contains(ifaces, netd.Iface{Name: iface, Tunnel: true}) {
			return fmt.Errorf("%s is not a tunnel interface on this computer", iface)
		}
	}
	return s.st.EditLocal(func(l *store.Local) error {
		l.VPNIfaces = setLocal(l.VPNIfaces, id, iface)
		return nil
	})
}

// conflict is R13's check before profile id connects system-wide: a plan
// cannot place a second full tunnel, or a network another connected,
// connecting or reconnecting system-wide VPN or connected external VPN
// routes.
func (s *RoutingService) conflict(id string) error {
	d, err := s.st.Data()
	if err != nil {
		return err
	}
	l, err := s.st.Local()
	if err != nil {
		return err
	}
	v := d.VPN(id)
	if v == nil {
		return errors.New("VPN profile not found")
	}
	var up []routing.Profile
	for _, oid := range s.vpns.active() {
		if o := d.VPN(oid); o != nil {
			up = append(up, profileFor(*o, l))
		}
	}
	return routing.Conflict(up, profileFor(*v, l))
}
