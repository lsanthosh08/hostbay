package app

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/monitor"
)

// MonitorService runs the Monitor tabs (R59, KTD29). The first tab on a host
// starts one sampler, which holds its own lease on the host's shared
// connection as TunnelService does; later tabs subscribe to it. It stops
// when its last tab stops or its tabs' sessions disconnect, and pauses
// while none of its tabs is visible.
type MonitorService struct {
	conns *ConnService
	emit  func(name string, data any)
	tick  <-chan time.Time // tests' ticks; nil uses the interval

	mu    sync.Mutex
	hosts map[string]*hostMonitor // host ID -> its sampler
	subs  map[string]*monitorSub  // monitor ID -> one tab
}

type hostMonitor struct {
	sampler *monitor.Sampler
	lease   string
	subs    map[string]*monitorSub
}

type monitorSub struct {
	host, session string
	visible       bool
}

// MonitorStart is what a tab starts from: its monitor ID for the other
// calls and for MonitorEvent, the sampler's state, its kept samples (only
// the newest carries processes and ports) and its interval in seconds.
type MonitorStart struct {
	ID       string           `json:"id"`
	State    monitor.Event    `json:"state"`
	History  []monitor.Sample `json:"history"`
	Interval int              `json:"interval"`
}

func NewMonitorService(conns *ConnService) *MonitorService {
	s := &MonitorService{conns: conns, emit: emitEvent, hosts: map[string]*hostMonitor{}, subs: map[string]*monitorSub{}}
	conns.onDisconnect(s.sessionClosed)
	return s
}

// Start subscribes a visible tab of sessionID to hostID's sampler, starting
// it if this is the host's first tab.
func (s *MonitorService) Start(ctx context.Context, sessionID, hostID string) (MonitorStart, error) {
	if _, err := s.conns.lease(sessionID); err != nil {
		return MonitorStart{}, err
	}
	s.mu.Lock()
	hm := s.hosts[hostID]
	if hm == nil {
		s.mu.Unlock()
		started, err := s.open(ctx, hostID)
		if err != nil {
			return MonitorStart{}, err
		}
		s.mu.Lock()
		if hm = s.hosts[hostID]; hm == nil {
			hm = started
			s.hosts[hostID] = hm
		} else { // another tab started one meanwhile
			defer s.close(started)
		}
	}
	id := newRequestID()
	sub := &monitorSub{host: hostID, session: sessionID, visible: true}
	hm.subs[id], s.subs[id] = sub, sub
	hm.sampler.SetPaused(false)
	out := MonitorStart{ID: id, State: hm.sampler.Last(), History: hm.sampler.History(), Interval: int(hm.sampler.Interval() / time.Second)}
	s.mu.Unlock()
	return out, nil
}

// open takes a lease on the host and starts a sampler on it, fanning its
// events out to the host's tabs.
func (s *MonitorService) open(ctx context.Context, hostID string) (*hostMonitor, error) {
	lease, err := s.conns.Connect(ctx, hostID)
	if err != nil {
		return nil, err
	}
	l, err := s.conns.lease(lease)
	if err != nil {
		s.conns.Disconnect(lease)
		return nil, err
	}
	hm := &hostMonitor{lease: lease, subs: map[string]*monitorSub{}}
	hm.sampler = monitor.New(monitor.Config{
		// Client, not Live: after a drop the monitor waits for the session's
		// reconnect instead of rebuilding the connection itself.
		Client: func() *ssh.Client {
			if cl := l.Client(); cl != nil {
				return cl.Client
			}
			return nil
		},
		Emit: func(e monitor.Event) {
			s.mu.Lock()
			ids := slices.Collect(maps.Keys(hm.subs))
			s.mu.Unlock()
			for _, id := range ids {
				s.emit(MonitorEvent(id), e)
			}
		},
		Tick: s.tick,
	})
	return hm, nil
}

// close stops a sampler that has no tabs left and releases its lease. Call
// it without s.mu: the sampler's last emit may be waiting for it.
func (s *MonitorService) close(hm *hostMonitor) {
	hm.sampler.Stop()
	s.conns.Disconnect(hm.lease)
}

// remove drops tabs; with s.mu held. It returns the samplers left without
// tabs, for close, and pauses the rest if none of their tabs shows.
func (s *MonitorService) remove(ids ...string) []*hostMonitor {
	var idle []*hostMonitor
	for _, id := range ids {
		sub := s.subs[id]
		if sub == nil {
			continue
		}
		delete(s.subs, id)
		hm := s.hosts[sub.host]
		delete(hm.subs, id)
		if len(hm.subs) == 0 {
			delete(s.hosts, sub.host)
			idle = append(idle, hm)
		} else {
			s.repause(hm)
		}
	}
	return idle
}

func (s *MonitorService) repause(hm *hostMonitor) {
	for _, sub := range hm.subs {
		if sub.visible {
			hm.sampler.SetPaused(false)
			return
		}
	}
	hm.sampler.SetPaused(true)
}

// Stop ends one tab's subscription; the host's last tab stops its sampler.
func (s *MonitorService) Stop(monitorID string) error {
	s.mu.Lock()
	idle := s.remove(monitorID)
	s.mu.Unlock()
	for _, hm := range idle {
		s.close(hm)
	}
	return nil
}

// SetVisible records whether a tab shows; a sampler none of whose tabs
// shows pauses, because hidden tabs stay mounted (KTD16).
func (s *MonitorService) SetVisible(monitorID string, visible bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[monitorID]
	if sub == nil {
		return errors.New("the monitor has stopped")
	}
	sub.visible = visible
	s.repause(s.hosts[sub.host])
	return nil
}

// SetInterval sets the host's sampling interval for all its tabs, clamped
// to 2–30 seconds, and returns the interval in force.
func (s *MonitorService) SetInterval(monitorID string, seconds int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.subs[monitorID]
	if sub == nil {
		return 0, errors.New("the monitor has stopped")
	}
	return int(s.hosts[sub.host].sampler.SetInterval(time.Duration(seconds)*time.Second) / time.Second), nil
}

// sessionClosed is ConnService's Disconnect hook: the session's tabs stop.
func (s *MonitorService) sessionClosed(sessionID string) {
	s.mu.Lock()
	var ids []string
	for id, sub := range s.subs {
		if sub.session == sessionID {
			ids = append(ids, id)
		}
	}
	idle := s.remove(ids...)
	s.mu.Unlock()
	for _, hm := range idle {
		s.close(hm)
	}
}

// ServiceShutdown stops every sampler when the app quits.
func (s *MonitorService) ServiceShutdown() error {
	s.mu.Lock()
	idle := s.remove(slices.Collect(maps.Keys(s.subs))...)
	s.mu.Unlock()
	for _, hm := range idle {
		s.close(hm)
	}
	return nil
}
