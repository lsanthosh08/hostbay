package app

import (
	"context"
	"errors"
	"sync"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
	"hostbay/internal/tunnel"
)

// TunnelStateEvent carries a TunnelState each time a tunnel's state changes.
const TunnelStateEvent = "tunnel-state"

// Tunnel is a saved port forward on a host (R23); see store.Tunnel.
type Tunnel struct {
	ID            string `json:"id"`
	HostID        string `json:"hostId"`
	Name          string `json:"name"`
	Kind          string `json:"kind"` // local | remote | dynamic
	BindHost      string `json:"bindHost"`
	BindPort      int    `json:"bindPort"`
	TargetHost    string `json:"targetHost"`
	TargetPort    int    `json:"targetPort"`
	AllowExternal bool   `json:"allowExternal"`
}

// Tunnel statuses. Only starting and listening hold a connection.
const (
	TunnelStarting  = "starting"
	TunnelListening = "listening"
	TunnelFailed    = "failed"  // could not start; Error says why
	TunnelDropped   = "dropped" // the connection ended; not restarted (R33)
	TunnelStopped   = "stopped"
)

// TunnelState is a tunnel's live state. Addr is where it listens (on the
// host for a remote tunnel).
type TunnelState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Addr   string `json:"addr,omitempty"`
	Error  string `json:"error,omitempty"`
}

// TunnelService saves tunnels and runs them. A running tunnel holds a lease
// on its host's shared connection (KTD5) and releases it when it stops or
// drops.
type TunnelService struct {
	st    *store.Store
	conns *ConnService
	emit  func(name string, data any)

	mu     sync.Mutex
	runs   map[string]*tunnelRun // tunnel ID -> starting or listening run
	states map[string]TunnelState
}

type tunnelRun struct {
	cancel  context.CancelFunc
	tn      *tunnel.Tunnel // nil while starting
	session string         // the lease ID, from ConnService.Connect
}

func NewTunnelService(st *store.Store, conns *ConnService) *TunnelService {
	return &TunnelService{st: st, conns: conns, emit: emitEvent, runs: map[string]*tunnelRun{}, states: map[string]TunnelState{}}
}

func tunnelSpec(t store.Tunnel) tunnel.Spec {
	return tunnel.Spec{
		Kind: tunnel.Kind(t.Kind), BindHost: t.BindHost, BindPort: t.BindPort,
		TargetHost: t.TargetHost, TargetPort: t.TargetPort, AllowExternal: t.AllowExternal,
	}
}

func (s *TunnelService) ListTunnels() ([]Tunnel, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	out := []Tunnel{}
	for _, t := range d.Tunnels.Items {
		out = append(out, Tunnel{
			ID: t.ID, HostID: t.HostID, Name: t.Name, Kind: t.Kind, BindHost: t.BindHost, BindPort: t.BindPort,
			TargetHost: t.TargetHost, TargetPort: t.TargetPort, AllowExternal: t.AllowExternal,
		})
	}
	return out, nil
}

// SaveTunnel adds or updates a tunnel. A running tunnel picks up the change
// the next time it starts.
func (s *TunnelService) SaveTunnel(t Tunnel) (Tunnel, error) {
	rec := store.Tunnel{
		Meta: store.Meta{ID: t.ID}, HostID: t.HostID, Name: t.Name, Kind: t.Kind, BindHost: t.BindHost, BindPort: t.BindPort,
		TargetHost: t.TargetHost, TargetPort: t.TargetPort, AllowExternal: t.AllowExternal,
	}
	if err := tunnelSpec(rec).Validate(); err != nil {
		return Tunnel{}, err
	}
	err := s.st.Edit(func(d *store.Data) error {
		if d.Host(t.HostID) == nil {
			return errors.New("host not found")
		}
		if old := d.Tunnel(t.ID); old != nil {
			rec.Meta = old.Meta
		}
		rec = d.PutTunnel(rec)
		return nil
	})
	t.ID = rec.ID
	return t, err
}

// DeleteTunnel stops the tunnel and removes it.
func (s *TunnelService) DeleteTunnel(id string) error {
	if err := s.StopTunnel(id); err != nil {
		return err
	}
	if err := s.st.Edit(func(d *store.Data) error { return d.PurgeTunnel(id) }); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.states, id)
	s.mu.Unlock()
	return nil
}

// TunnelStates lists every tunnel whose state is known; the rest are stopped.
func (s *TunnelService) TunnelStates() []TunnelState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []TunnelState{}
	for _, st := range s.states {
		out = append(out, st)
	}
	return out
}

// RunningTunnels counts tunnels starting or listening, so quitting can ask
// first (R33).
func (s *TunnelService) RunningTunnels() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// set records and announces a state; call it with s.mu held so events go
// out in order.
func (s *TunnelService) set(st TunnelState) TunnelState {
	s.states[st.ID] = st
	s.emit(TunnelStateEvent, st)
	return st
}

// StartTunnel connects to the tunnel's host (through its jump chain) and
// starts listening. A tunnel already starting or listening is left as is.
// Any failure shows as failed with the reason and is also returned, so the
// UI can answer a host-key prompt.
func (s *TunnelService) StartTunnel(ctx context.Context, id string) (TunnelState, error) {
	s.mu.Lock()
	if s.runs[id] != nil {
		defer s.mu.Unlock()
		return s.states[id], nil
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &tunnelRun{cancel: cancel}
	s.runs[id] = r
	s.set(TunnelState{ID: id, Status: TunnelStarting})
	s.mu.Unlock()

	tn, session, err := s.open(ctx, id)

	s.mu.Lock()
	if s.runs[id] != r { // stopped while starting
		st := s.states[id]
		s.mu.Unlock()
		cancel()
		if tn != nil {
			tn.Close()
			s.conns.Disconnect(session)
		}
		return st, nil
	}
	defer s.mu.Unlock()
	if err != nil {
		cancel()
		delete(s.runs, id)
		st := TunnelState{ID: id, Status: TunnelFailed, Error: err.Error()}
		if apperr.CodeOf(err) == apperr.HostKeyUnknown {
			st = TunnelState{ID: id, Status: TunnelStopped} // a prompt, not a failure
		}
		return s.set(st), err
	}
	r.tn, r.session = tn, session
	go s.watch(id, r)
	return s.set(TunnelState{ID: id, Status: TunnelListening, Addr: tn.Addr().String()}), nil
}

// open takes a lease on the tunnel's host and starts the tunnel on it. On
// failure the lease is released again.
func (s *TunnelService) open(ctx context.Context, id string) (*tunnel.Tunnel, string, error) {
	d, err := s.st.Data()
	if err != nil {
		return nil, "", err
	}
	t := d.Tunnel(id)
	if t == nil {
		return nil, "", errors.New("tunnel not found")
	}
	spec := tunnelSpec(*t)
	if err := spec.Validate(); err != nil {
		return nil, "", err
	}
	session, err := s.conns.Connect(ctx, t.HostID)
	if err != nil {
		return nil, "", err
	}
	l, err := s.conns.lease(session)
	if err != nil {
		return nil, "", err
	}
	cl, err := l.Live(ctx)
	var tn *tunnel.Tunnel
	if err == nil {
		tn, err = tunnel.Open(cl.Client, spec)
	}
	if err != nil {
		s.conns.Disconnect(session)
		return nil, "", err
	}
	return tn, session, nil
}

// watch shows a tunnel that ended by itself as dropped and releases its
// lease. It does not restart it (R33).
func (s *TunnelService) watch(id string, r *tunnelRun) {
	<-r.tn.Done()
	s.mu.Lock()
	if s.runs[id] != r { // stopped by the user
		s.mu.Unlock()
		return
	}
	delete(s.runs, id)
	st := TunnelState{ID: id, Status: TunnelStopped}
	if err := r.tn.Err(); err != nil {
		st = TunnelState{ID: id, Status: TunnelDropped, Error: err.Error()}
	}
	s.set(st)
	s.mu.Unlock()
	r.cancel()
	s.conns.Disconnect(r.session)
}

// StopTunnel closes the tunnel's listener and connections and releases its
// lease. Stopping a tunnel that is not running clears a failed or dropped
// state.
func (s *TunnelService) StopTunnel(id string) error {
	s.mu.Lock()
	r := s.runs[id]
	delete(s.runs, id)
	s.mu.Unlock()
	if r != nil {
		r.cancel()
		if r.tn != nil {
			r.tn.Close()
			s.conns.Disconnect(r.session)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, known := s.states[id]; known && s.runs[id] == nil {
		s.set(TunnelState{ID: id, Status: TunnelStopped})
	}
	return nil
}

// ServiceShutdown stops every tunnel when the app quits.
func (s *TunnelService) ServiceShutdown() error {
	s.mu.Lock()
	var ids []string
	for id := range s.runs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.StopTunnel(id)
	}
	return nil
}
