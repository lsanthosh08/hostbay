package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/netd"
)

// Helper states (R30), as Settings shows them; each but ready says why
// system-wide routing cannot run and what fixes it.
const (
	HelperUnavailable = "unavailable" // this build cannot install it; Reason says why
	HelperMissing     = "missing"     // not installed: Install
	HelperRepair      = "repair"      // paired with another user or Hostbay copy: Install re-pairs
	HelperStopped     = "stopped"     // installed and not answering
	HelperReinstall   = "reinstall"   // speaks another protocol version: Install replaces it
	HelperReady       = "ready"
)

// HelperStatus is the privileged helper's state.
type HelperStatus struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// netdClient is the part of *netd.Client Hostbay uses; tests use a fake.
type netdClient interface {
	Add(context.Context, netd.Route) error
	Delete(context.Context, netd.Route) error
	SetMTU(ctx context.Context, iface string, mtu int) error
	Events() <-chan struct{}
	Done() <-chan struct{}
	Close() error
}

var (
	// netdRetryMin and netdRetryMax bound the wait between attempts to reach
	// the helper; Install retries at once.
	netdRetryMin = time.Second
	netdRetryMax = time.Minute
	// netdBusyWait covers the moment after a disconnect when the helper
	// still holds the old connection and answers busy.
	netdBusyWait = 200 * time.Millisecond
)

// NetdService installs hostbay-netd (KTD8) and keeps Hostbay's one
// connection to it. The helper sweeps every route a connection made when it
// closes, so Hostbay's routes live only while Hostbay runs (KTD9).
type NetdService struct {
	status    func(context.Context) netd.Status
	dial      func(context.Context) (netdClient, error)
	install   func() error
	uninstall func() error

	// Set before startup: onConnect runs on every (re)connect, so the
	// routing engine re-applies; onEvent on every route table change.
	onConnect func()
	onEvent   func()

	mu        sync.Mutex
	c         netdClient
	connected chan struct{} // closed and replaced on every connect
	wake      chan struct{}
	stop      context.CancelFunc
	exited    chan struct{} // closed when the connection loop has ended
}

func NewNetdService() *NetdService {
	return &NetdService{
		status:  netd.GetStatus,
		dial:    func(ctx context.Context) (netdClient, error) { return netd.Dial(ctx) },
		install: netd.Install, uninstall: netd.Uninstall,
		connected: make(chan struct{}), wake: make(chan struct{}, 1),
	}
}

// helperStatus maps the helper's status to what to offer. Current is false
// whenever the helper does not answer, so a stopped helper is told apart
// before a version mismatch.
func helperStatus(st netd.Status) HelperStatus {
	switch {
	case st.Unavailable != "":
		return HelperStatus{HelperUnavailable, st.Unavailable}
	case !st.Installed:
		return HelperStatus{State: HelperMissing}
	case !st.Paired:
		return HelperStatus{State: HelperRepair}
	case !st.Running:
		return HelperStatus{State: HelperStopped}
	case !st.Current:
		return HelperStatus{State: HelperReinstall}
	}
	return HelperStatus{State: HelperReady}
}

// Status is the helper's state now.
func (s *NetdService) Status(ctx context.Context) HelperStatus {
	if c := s.client(); c != nil {
		// Connected, but to a copy made by an older build.
		if sc, ok := c.(interface{ Stale() bool }); ok && sc.Stale() {
			return HelperStatus{State: HelperReinstall}
		}
		return HelperStatus{State: HelperReady}
	}
	return helperStatus(s.status(ctx))
}

// Install installs, reinstalls or re-pairs the helper after one native
// administrator prompt that says what it can do (R30).
func (s *NetdService) Install() (HelperStatus, error) {
	err := s.install()
	if err == nil {
		s.drop() // a connection to the helper this replaced
	}
	return s.Status(context.Background()), err
}

// Uninstall removes the helper; the connection ends first, so it sweeps
// Hostbay's routes.
func (s *NetdService) Uninstall() (HelperStatus, error) {
	s.drop()
	err := s.uninstall()
	return s.Status(context.Background()), err
}

func (s *NetdService) client() netdClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c
}

// drop ends the connection and reconnects at once: the helper sweeps every
// route it made.
func (s *NetdService) drop() {
	if c := s.client(); c != nil {
		c.Close()
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// reconnect is drop, waiting until the new connection is up.
func (s *NetdService) reconnect(ctx context.Context) error {
	s.mu.Lock()
	next := s.connected
	s.mu.Unlock()
	s.drop()
	select {
	case <-next:
		return nil
	case <-ctx.Done():
		return errors.New("Hostbay could not reconnect to its network helper")
	}
}

func (s *NetdService) ServiceStartup(context.Context, application.ServiceOptions) error {
	ctx, stop := context.WithCancel(context.Background())
	s.stop, s.exited = stop, make(chan struct{})
	go func() {
		defer close(s.exited)
		s.keep(ctx)
	}()
	return nil
}

// ServiceShutdown closes the connection, so the helper removes Hostbay's
// routes and restores the vendor routes it narrowed.
func (s *NetdService) ServiceShutdown() error {
	if s.stop != nil {
		s.stop()
		<-s.exited
	}
	if c := s.client(); c != nil {
		c.Close()
	}
	return nil
}

// keep holds a connection to the helper while Hostbay runs, reconnecting
// after it drops.
func (s *NetdService) keep(ctx context.Context) {
	wait := netdRetryMin
	for ctx.Err() == nil {
		if c, err := s.connect(ctx); err == nil {
			wait = netdRetryMin
			s.mu.Lock()
			s.c = c
			close(s.connected)
			s.connected = make(chan struct{})
			s.mu.Unlock()
			if s.onConnect != nil {
				s.onConnect()
			}
			s.serve(ctx, c)
			s.mu.Lock()
			s.c = nil
			s.mu.Unlock()
		}
		select {
		case <-ctx.Done():
		case <-s.wake:
			wait = netdRetryMin
		case <-time.After(wait):
			wait = min(wait*2, netdRetryMax)
		}
	}
}

// connect dials, waiting out a helper still busy with the last connection.
func (s *NetdService) connect(ctx context.Context) (netdClient, error) {
	for range 10 {
		c, err := s.dial(ctx)
		var e *netd.Error
		if !errors.As(err, &e) || e.Code != netd.CodeBusy {
			return c, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(netdBusyWait):
		}
	}
	return nil, errors.New("the network helper stayed busy")
}

func (s *NetdService) serve(ctx context.Context, c netdClient) {
	for {
		select {
		case <-ctx.Done():
			c.Close()
			return
		case <-c.Done():
			return
		case <-c.Events():
			if s.onEvent != nil {
				s.onEvent()
			}
		}
	}
}
