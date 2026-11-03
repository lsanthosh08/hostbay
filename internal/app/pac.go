package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/store"
	"hostbay/internal/vpn/pac"
	"hostbay/internal/vpn/routing"
)

// PAC states, as Settings and the VPN panel show them.
const (
	PACOff    = "off"    // no connected app-scoped VPN has sites or networks
	PACSet    = "set"    // the system's automatic proxy is Hostbay's PAC
	PACHelper = "helper" // there is something to route, but the network helper is not connected
	PACFailed = "failed" // the helper refused the change; Reason says why
	// PACOverridden: Hostbay's PAC is set, but the configuration macOS
	// applies is another one, such as a VPN client's or a managed profile's.
	PACOverridden = "overridden"
)

// PACStateEvent carries the PACState whenever it changes.
const PACStateEvent = "pac-state"

var (
	// pacDebounce gathers a burst of VPN state changes into one rebuild.
	pacDebounce = 200 * time.Millisecond
	// pacRetry is how soon a rebuild runs again after a listener or the
	// helper failed, with nothing else changing.
	pacRetry = 5 * time.Second
)

// PACState is what the PAC routes, per profile ID. It never carries the PAC's
// address or a listener port, which stay out of the UI.
type PACState struct {
	State    string         `json:"state"`
	Reason   string         `json:"reason,omitempty"`
	Domains  map[string]int `json:"domains"`
	Networks map[string]int `json:"networks"`
}

// PACService serves the proxy auto-config that sends each connected
// app-scoped VPN's sites and networks to its SOCKS listener, and keeps the
// system's automatic proxy pointed at it through hostbay-netd while there is
// anything to route. Browsers fetch a PAC again only at a new address, so
// every change gets a new version in the path.
type PACService struct {
	st   *store.Store
	vpns *VPNService
	netd *NetdService
	// port asks a connected profile's helper for its no-login SOCKS
	// listener; tests replace it.
	port func(ctx context.Context, id string) (int, error)
	// effective is the automatic proxy macOS applies now; tests replace it.
	effective func() (url string, on bool, err error)
	// local is this computer's own networks, which stay direct; tests
	// replace it.
	local func() []netip.Prefix
	emit  func(name string, data any)

	kicks  chan struct{}
	token  string
	srv    *http.Server
	addr   int // the server's port
	stop   context.CancelFunc
	exited chan struct{}

	mu      sync.Mutex
	script  string
	version int
	set     string             // the URL the helper last set; "" when none is
	conn    int                // counts helper connects and disconnects, each of which unsets set
	ports   map[string]pacPort // profile ID: its listener while connected since
	state   PACState
	sent    PACState // the state last emitted, as State shows it
}

type pacPort struct {
	since int64
	port  int
}

// NewPACService links the PAC to VPN states, profile edits and the helper
// connection.
func NewPACService(st *store.Store, vpns *VPNService, nd *NetdService) *PACService {
	s := &PACService{st: st, vpns: vpns, netd: nd, port: vpns.browserPort, effective: effectiveAutoProxy, local: localNetworks(st), emit: emitEvent,
		kicks: make(chan struct{}, 1), ports: map[string]pacPort{}, state: PACState{State: PACOff, Domains: map[string]int{}, Networks: map[string]int{}}}
	// The helper sweeps the setting when its connection ends, so a new
	// connection needs it set again, and until then nothing is set.
	unset := func() {
		s.mu.Lock()
		s.set = ""
		s.conn++
		s.mu.Unlock()
		s.kick()
	}
	connect, disconnect := nd.onConnect, nd.onDisconnect
	nd.onConnect = func() {
		if connect != nil {
			connect()
		}
		unset()
	}
	nd.onDisconnect = func() {
		if disconnect != nil {
			disconnect()
		}
		unset()
	}
	// VPNService emits with its lock held, so this only signals.
	emit := vpns.emit
	vpns.emit = func(name string, data any) {
		emit(name, data)
		if name == VPNStateEvent {
			s.kick()
		}
	}
	saved := vpns.saved
	vpns.saved = func(id string) {
		if saved != nil {
			saved(id)
		}
		s.kick()
	}
	return s
}

func (s *PACService) ServiceStartup(context.Context, application.ServiceOptions) error {
	b := make([]byte, 24)
	rand.Read(b)
	s.token = base64.RawURLEncoding.EncodeToString(b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("serving the proxy configuration: %w", err)
	}
	s.addr = ln.Addr().(*net.TCPAddr).Port
	s.srv = &http.Server{Handler: pacGuard(s.addr, http.HandlerFunc(s.serve)), ReadHeaderTimeout: 5 * time.Second}
	go s.srv.Serve(ln)
	ctx, stop := context.WithCancel(context.Background())
	s.stop, s.exited = stop, make(chan struct{})
	go func() {
		defer close(s.exited)
		s.loop(ctx)
	}()
	s.kick()
	return nil
}

// ServiceShutdown puts the system's proxy back before the helper connection
// closes; after a crash the helper's sweep does.
func (s *PACService) ServiceShutdown() error {
	if s.stop != nil {
		s.stop()
		<-s.exited
	}
	s.mu.Lock()
	set := s.set
	s.set = ""
	s.mu.Unlock()
	if c := s.netd.client(); c != nil && set != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.ClearProxy(ctx); err != nil {
			log.Printf("hostbay: clearing the system proxy: %v", err)
		}
	}
	if s.srv != nil {
		s.srv.Close()
	}
	return nil
}

// pacGuard lets a request through only when it names this listener exactly
// as its Host and carries no Origin, so a web page, even one reached by DNS
// rebinding, cannot read which sites go through which VPN.
func pacGuard(port int, next http.Handler) http.Handler {
	host := "127.0.0.1:" + strconv.Itoa(port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Origin"]; ok || r.Host != host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serve answers any version at the token's path with the current script: a
// browser still on an older address gets today's rules.
func (s *PACService) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/hostbay-pac/"+s.token+"/")
	if !ok || !strings.HasSuffix(rest, ".pac") || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	script := s.script
	s.mu.Unlock()
	if script == "" {
		script = "function FindProxyForURL(url, host) {\n  return \"DIRECT\";\n}\n"
	}
	w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, script)
}

func (s *PACService) kick() {
	select {
	case s.kicks <- struct{}{}:
	default:
	}
}

func (s *PACService) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kicks:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pacDebounce):
		}
		select {
		case <-s.kicks:
		default:
		}
		s.rebuild(ctx)
	}
}

// rebuild writes the PAC from the connected app-scoped profiles and points
// the system at it, or clears the setting when nothing is left to route.
// Without the helper it opens no listener: nothing could reach it.
func (s *PACService) rebuild(ctx context.Context) {
	d, err := s.st.Data()
	if err != nil {
		return
	}
	states := map[string]VPNState{}
	for _, st := range s.vpns.VPNStates() {
		states[st.ID] = st
	}
	c := s.netd.client()
	retry := false
	var profiles []pac.Profile
	live := map[string]bool{}
	for _, v := range d.VPNs.Items { // saved order breaks ties
		st := states[v.ID]
		if st.Status != VPNConnected || vpnTier(v) != store.VPNTierApp {
			continue
		}
		nets := slices.Concat(v.Routes, st.Routes)
		if len(v.Domains) == 0 && len(nets) == 0 {
			continue
		}
		live[v.ID] = true
		port := 0 // counted only, until the helper is back
		if c != nil {
			if port, err = s.listener(ctx, v.ID, st.Since); err != nil {
				retry = true
				continue // its sites go direct until its listener answers
			}
		}
		profiles = append(profiles, pac.Profile{ID: v.ID, Name: v.Name, Port: port, Domains: v.Domains, Networks: nets})
	}
	res := pac.Build(profiles, s.local()...)

	s.mu.Lock()
	maps.DeleteFunc(s.ports, func(id string, _ pacPort) bool { return !live[id] })
	// Without the helper the script stays as it was, so a quick reconnect
	// sets the same address again.
	if c != nil && res.Script != s.script {
		s.script = res.Script
		s.version++
	}
	url := ""
	if c != nil && s.script != "" {
		url = fmt.Sprintf("http://127.0.0.1:%d/hostbay-pac/%s/%d.pac", s.addr, s.token, s.version)
	}
	set, conn := s.set, s.conn
	s.mu.Unlock()

	next := PACState{State: PACOff, Domains: res.Domains, Networks: res.Networks}
	var applyErr error
	switch {
	case c == nil:
		if len(profiles) > 0 {
			next.State = PACHelper
		}
	case url != set:
		actx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if url == "" {
			applyErr = c.ClearProxy(actx)
		} else {
			applyErr = c.SetProxy(actx, url)
		}
		cancel()
		if applyErr == nil {
			set = url
		}
	}
	if applyErr != nil {
		next.State, next.Reason, retry = PACFailed, applyErr.Error(), true
	} else if url != "" && url == set {
		next.State = PACSet
	}
	s.mu.Lock()
	if s.conn == conn {
		s.set = set
	} else {
		retry = true // the helper connected or left meanwhile
	}
	s.state = next
	s.mu.Unlock()
	if retry {
		time.AfterFunc(pacRetry, s.kick)
	}
	shown := s.State()
	s.mu.Lock()
	changed := !pacStateEqual(s.sent, shown)
	s.sent = shown
	s.mu.Unlock()
	if changed {
		s.emit(PACStateEvent, shown)
	}
}

// listener is profile id's SOCKS port, asked of its helper once per
// connection: each connect opens a new one.
func (s *PACService) listener(ctx context.Context, id string, since int64) (int, error) {
	s.mu.Lock()
	p, ok := s.ports[id]
	s.mu.Unlock()
	if ok && p.since == since {
		return p.port, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	port, err := s.port(ctx, id)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.ports[id] = pacPort{since, port}
	s.mu.Unlock()
	return port, nil
}

func pacStateEqual(a, b PACState) bool {
	return a.State == b.State && a.Reason == b.Reason && maps.Equal(a.Domains, b.Domains) && maps.Equal(a.Networks, b.Networks)
}

// State is what the PAC routes now, and whether the system applies it.
func (s *PACService) State() PACState {
	s.mu.Lock()
	st, set := s.state, s.set
	st.Domains, st.Networks = maps.Clone(st.Domains), maps.Clone(st.Networks)
	s.mu.Unlock()
	if st.State == PACSet {
		// Only the primary service's settings apply, so another app's or a
		// managed profile's configuration can win.
		if url, on, err := s.effective(); err == nil && (!on || url != set) {
			st.State = PACOverridden
		}
	}
	return st
}

// localNetworks is the IPv4 networks of this computer's interfaces other
// than tunnels, and the destinations the user sends to the local network.
func localNetworks(st *store.Store) func() []netip.Prefix {
	return func() []netip.Prefix {
		var out []netip.Prefix
		if l, err := st.Local(); err == nil {
			for _, d := range l.RoutingLocal {
				if p, err := routing.ParseDst(d); err == nil {
					out = append(out, p)
				}
			}
		}
		ifs, _ := net.Interfaces()
		for _, i := range ifs {
			if i.Flags&net.FlagUp == 0 || i.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
				continue
			}
			addrs, _ := i.Addrs()
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok {
					ones, _ := n.Mask.Size()
					if ip, ok := netip.AddrFromSlice(n.IP.To4()); ok && n.IP.To4() != nil {
						out = append(out, netip.PrefixFrom(ip, ones).Masked())
					}
				}
			}
		}
		return out
	}
}

// browserPort is profile id's no-login SOCKS listener, asked of its helper
// outside the service lock. The port must not reach the UI, an event, a log
// or MCP.
func (s *VPNService) browserPort(ctx context.Context, id string) (int, error) {
	s.mu.Lock()
	var r *vpnRun
	if r = s.runs[id]; r == nil || r.att == nil || r.att.h == nil {
		s.mu.Unlock()
		return 0, errors.New("this VPN profile is not connected")
	}
	h := r.att.h
	s.mu.Unlock()
	return h.BrowserPort(ctx)
}
