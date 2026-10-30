// Package tailnet is hostbay-vpn's Tailscale and Headscale engine (KTD3):
// the profile joins its tailnet as its own node through tsnet, inside the
// helper's process, so it changes no system route, DNS setting or interface
// (R14). Only cmd/hostbay-vpn imports it, which keeps Tailscale out of the
// main binary.
//
// The node's keys live only in memory. Each time they change the engine
// reports the whole store as a nodeState message; Hostbay seals it, keeps it
// on this device, and sends it back as secrets["nodeState"] on the next
// start. The profile's folder holds only what tsnet writes besides that.
package tailnet

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"

	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/logtail"
	"tailscale.com/net/tsaddr"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"

	"hostbay/internal/vpn/proto"
)

type config struct {
	// Dir is the profile's own folder, which Hostbay deletes with the
	// profile. Two profiles never share one.
	Dir string `json:"dir"`
	// ControlURL is the Headscale server; empty is Tailscale's.
	ControlURL string `json:"controlURL"`
	// Hostname is the node's name in the tailnet.
	Hostname string `json:"hostname"`
}

// Engine is one tailnet node. New prepares it, Run brings it up, and Dial
// reaches hosts through it.
type Engine struct {
	srv   *tsnet.Server
	store *store
}

// New reads the profile's JSON configuration. secrets["authKey"] joins a new
// node without a browser sign-in; secrets["nodeState"] is a store an earlier
// run reported, which rejoins as the same node.
func New(conf string, secrets map[string]string) (*Engine, error) {
	var cfg config
	if err := json.Unmarshal([]byte(conf), &cfg); err != nil {
		return nil, errors.New("The Tailscale profile settings are not valid JSON.")
	}
	if cfg.Dir == "" {
		return nil, errors.New("The Tailscale profile has no state folder.")
	}
	// Before any node exists, for the whole process: nothing it logs goes
	// to Tailscale's log service, and the node says so to its tailnet.
	logtail.Disable()
	envknob.SetNoLogsNoSupport()
	// Otherwise the backend makes a log folder under the home folder, outside
	// the profile's (tsnet creates Dir before the backend reads this).
	os.Setenv("TS_LOGS_DIR", cfg.Dir)
	// tsnet falls back to these when the profile leaves a field empty, and
	// the helper inherits Hostbay's environment: a stray TS_CONTROL_URL or
	// TS_AUTHKEY in the user's shell would join another tailnet.
	for _, k := range []string{"TS_AUTHKEY", "TS_AUTH_KEY", "TS_CONTROL_URL", "TS_CLIENT_SECRET",
		"TS_CLIENT_ID", "TS_ID_TOKEN", "TS_AUDIENCE", "TSNET_FORCE_LOGIN"} {
		os.Unsetenv(k)
	}
	st := new(store)
	if s := secrets["nodeState"]; s != "" {
		b, err := base64.StdEncoding.DecodeString(s)
		if err == nil {
			err = st.LoadFromJSON(b)
		}
		if err != nil {
			return nil, errors.New("The saved Tailscale sign-in cannot be read. Sign in again.")
		}
	}
	hostname := cfg.Hostname
	if hostname == "" {
		hostname = "hostbay"
	}
	return &Engine{store: st, srv: &tsnet.Server{
		Dir:        cfg.Dir,
		Store:      st,
		Hostname:   hostname,
		ControlURL: cfg.ControlURL,
		AuthKey:    secrets["authKey"],
		// tsnet's default prints the sign-in URL to stderr, which Hostbay
		// logs; it goes to Hostbay in a need message instead.
		UserLogf: logger.Discard,
	}}, nil
}

// store is an in-memory ipn.StateStore that reports its whole content after
// each write. It is not a *mem.Store, which tsnet allows only for ephemeral
// nodes: those are deleted when they go offline, and this node lives on in
// Hostbay's sealed copy.
type store struct {
	mem.Store
	mu      sync.Mutex // writes and their reports, in order
	changed func(json []byte)
}

func (s *store) WriteState(id ipn.StateKey, bs []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Store.WriteState(id, bs)
	if s.changed != nil {
		b, _ := s.ExportToJSON()
		s.changed(b)
	}
	return nil
}

func (s *store) onChange(f func(json []byte)) {
	s.mu.Lock()
	s.changed = f
	s.mu.Unlock()
}

// Run starts the node and reports connecting, need (a browser sign-in) when
// the node has no auth key or saved state, and connected once it is running.
// Each change to the node's keys is reported as nodeState. It returns after
// reporting failed (the control server refused the node) or dropped (the
// tailnet signed it out), or when ctx ends.
func (e *Engine) Run(ctx context.Context, report func(proto.Msg)) {
	report(proto.Msg{T: proto.State, State: proto.Connecting})
	fail := func(reason string) { report(proto.Msg{T: proto.State, State: proto.Failed, Reason: reason}) }
	e.store.onChange(func(b []byte) {
		report(proto.Msg{T: proto.NodeState, NodeState: base64.StdEncoding.EncodeToString(b)})
	})
	defer e.srv.Close()
	if err := e.srv.Start(); err != nil {
		fail("Cannot start Tailscale: " + err.Error())
		return
	}
	lc, err := e.srv.LocalClient()
	if err != nil {
		fail("Cannot start Tailscale: " + err.Error())
		return
	}
	w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		fail("Cannot start Tailscale: " + err.Error())
		return
	}
	defer w.Close()

	up := false
	for {
		n, err := w.Next()
		if err != nil {
			return // ctx ended
		}
		if n.ErrMessage != nil && !up {
			fail("The tailnet refused this device: " + *n.ErrMessage)
			return
		}
		if n.BrowseToURL != nil && *n.BrowseToURL != "" {
			report(proto.Msg{T: proto.Need, Need: proto.NeedBrowser, URL: *n.BrowseToURL})
		}
		if n.State == nil {
			continue
		}
		switch s := *n.State; {
		case s == ipn.Running && !up:
			st, err := lc.Status(ctx)
			if err != nil {
				return // ctx ended
			}
			up = true
			var addr []string
			for _, a := range st.TailscaleIPs {
				addr = append(addr, netip.PrefixFrom(a, a.BitLen()).String())
			}
			routes := []string{tsaddr.CGNATRange().String(), tsaddr.TailscaleULARange().String()}
			for _, p := range st.Peer {
				if p.PrimaryRoutes != nil {
					for _, r := range p.PrimaryRoutes.All() {
						routes = append(routes, r.String())
					}
				}
			}
			report(proto.Msg{T: proto.State, State: proto.Connected, Address: addr, Routes: routes})
		case up && (s == ipn.NeedsLogin || s == ipn.NeedsMachineAuth || s == ipn.Stopped):
			report(proto.Msg{T: proto.State, State: proto.Dropped, Reason: "The tailnet signed this device out. Sign in again."})
			return
		}
	}
}

// Dial opens a TCP connection, or a connected UDP socket for network "udp",
// through the tailnet. Names resolve through the tailnet's DNS (MagicDNS).
func (e *Engine) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	return e.srv.Dial(ctx, network, address)
}
