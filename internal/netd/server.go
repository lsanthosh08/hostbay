package netd

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
)

// Tunnels runs system-wide tunnels for the helper (U12). Start gets a
// request validate passed and returns the reply's fields once the tunnel
// is launched, not once it is up: event then reports it up or ended.
type Tunnels interface {
	Start(m Msg, event func(Msg)) (Msg, *Error)
	Stop(handle string) *Error
	StopAll()
}

// Server is the helper's side of the protocol. It serves one Hostbay at a
// time and, when that Hostbay goes away, stops its tunnels and sweeps the
// journal.
type Server struct {
	Table   Table
	Journal *Journal
	// Tunnels is nil in a helper that runs none.
	Tunnels Tunnels
	// Auth admits or refuses a connection before the helper reads from it.
	Auth func(net.Conn) error

	opMu    sync.Mutex // one route change or sweep at a time
	stopped bool       // set by Stop under opMu: no change may follow the last sweep

	mu  sync.Mutex
	sub chan struct{} // the connected client's events; nil when none is
}

// Serve accepts connections until ln is closed.
func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

// Notify tells the connected client that the route table changed. Events
// coalesce: a client that has not read the last one gets no second.
func (s *Server) Notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sub != nil {
		select {
		case s.sub <- struct{}{}:
		default:
		}
	}
}

// Stop sweeps for the last time and refuses every later change, so a request
// still in flight when the helper stops cannot leave a route behind.
func (s *Server) Stop() {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.stopped = true
	if s.Tunnels != nil {
		s.Tunnels.StopAll()
	}
	if err := Sweep(s.Table, s.Journal); err != nil {
		log.Printf("hostbay-netd: sweep: %v", err)
	}
}

// Sweep undoes every journaled change.
func (s *Server) Sweep() {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := Sweep(s.Table, s.Journal); err != nil {
		log.Printf("hostbay-netd: sweep: %v", err)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	var wmu sync.Mutex // one message per write
	enc := json.NewEncoder(c)
	send := func(m Msg) error {
		wmu.Lock()
		defer wmu.Unlock()
		m.V = Version
		return enc.Encode(m)
	}

	if err := s.Auth(c); err != nil {
		log.Printf("hostbay-netd: refused a caller: %v", err)
		send(Msg{T: Reply, Code: CodeDenied, Reason: err.Error()})
		return
	}
	sub := make(chan struct{}, 1)
	s.mu.Lock()
	busy := s.sub != nil
	if !busy {
		s.sub = sub
	}
	s.mu.Unlock()
	if busy {
		send(Msg{T: Reply, Code: CodeBusy, Reason: "Another Hostbay is connected to the helper."})
		return
	}
	defer func() {
		if s.Tunnels != nil {
			s.Tunnels.StopAll() // before the sweep: their routes go with their interfaces
		}
		s.Sweep()
		s.mu.Lock()
		s.sub = nil
		s.mu.Unlock()
	}()

	if send(Msg{T: Hello}) != nil {
		return
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-sub:
				send(Msg{T: Event})
			case <-done:
				return
			}
		}
	}()

	dec := json.NewDecoder(c)
	for {
		var m Msg
		if dec.Decode(&m) != nil {
			return
		}
		if m.V != Version {
			send(Msg{T: Reply, ID: m.ID, Code: CodeVersion,
				Reason: fmt.Sprintf("The helper speaks protocol %d and Hostbay %d; reinstall the helper.", Version, m.V)})
			return
		}
		r, err := s.do(m, func(e Msg) { e.T = TunnelEvent; send(e) })
		r.T, r.ID = Reply, m.ID
		if err != nil {
			r.Code, r.Reason = err.Code, err.Reason
		}
		if send(r) != nil {
			return
		}
	}
}

// do performs one request within the helper's bounds (KTD8); a tunnel's
// events go to event.
func (s *Server) do(m Msg, event func(Msg)) (Msg, *Error) {
	switch m.T {
	case OpenVPNStart, OpenConnectStart, WireGuardStart, StopTunnel:
		if s.Tunnels == nil {
			return Msg{}, &Error{CodeUnsupported, "This helper cannot run system-wide VPNs."}
		}
		if err := validate(m); err != nil {
			return Msg{}, err
		}
		if m.T == StopTunnel {
			return Msg{}, s.Tunnels.Stop(m.Tunnel)
		}
		s.opMu.Lock()
		stopped := s.stopped
		s.opMu.Unlock()
		if stopped {
			return Msg{}, refused("The helper is stopping.")
		}
		return s.Tunnels.Start(m, event)
	}
	return Msg{}, s.route(m)
}

// route performs a route or MTU change.
func (s *Server) route(m Msg) *Error {
	if err := validate(m); err != nil {
		return err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.stopped {
		return refused("The helper is stopping.")
	}
	ifaces, err := s.Table.Interfaces()
	if err != nil {
		return failed("reading interfaces: %v", err)
	}
	tunnel := map[string]bool{}
	exists := map[string]bool{}
	for _, i := range ifaces {
		exists[i.Name] = true
		tunnel[i.Name] = i.Tunnel
	}
	switch m.T {
	case Add:
		return s.add(*m.Route, tunnel, exists)
	case Delete:
		return s.delete(*m.Route, tunnel)
	default: // SetMTU
		if !tunnel[m.Iface] {
			return refused("%s is not a tunnel interface.", m.Iface)
		}
		if err := s.Table.SetMTU(m.Iface, m.MTU); err != nil {
			return failed("setting the MTU of %s: %v", m.Iface, err)
		}
		return nil
	}
}

// add adds a route only through a tunnel interface or the local default
// gateway, journaling it first.
func (s *Server) add(req Route, tunnel, exists map[string]bool) *Error {
	dst, _ := parseCIDR(req.Dst)
	r := SysRoute{Dst: dst, Iface: req.Iface}
	if req.Gateway {
		routes, err := s.Table.Routes()
		if err != nil {
			return failed("reading routes: %v", err)
		}
		gw, ok := defaultGateway(routes, tunnel, dst)
		if !ok {
			return refused("There is no local default gateway for %s.", dst)
		}
		r.Iface, r.Gateway = gw.Iface, gw.Gateway
	} else if !exists[r.Iface] {
		return refused("There is no interface %s.", r.Iface)
	} else if !tunnel[r.Iface] {
		return refused("%s is not a tunnel interface; routes go through a tunnel or the default gateway.", r.Iface)
	}
	if err := s.Journal.record(Added, r); err != nil {
		return failed("journaling %s: %v", r, err)
	}
	if err := s.Table.Add(r); err != nil {
		s.Journal.forget(Added, r)
		return failed("adding %s: %v", r, err)
	}
	return nil
}

// delete deletes a route the helper added, or a vendor route on a tunnel
// interface, journaling the vendor route first so the sweep can restore it.
func (s *Server) delete(req Route, tunnel map[string]bool) *Error {
	if r, ok := s.Journal.added(req); ok {
		if err := s.Table.Delete(r); err != nil {
			return failed("deleting %s: %v", r, err)
		}
		s.Journal.forget(Added, r)
		return nil
	}
	if req.Gateway {
		return refused("Hostbay added no route to %s through the default gateway.", req.Dst)
	}
	if !tunnel[req.Iface] {
		return refused("%s on %s was not added by Hostbay and is not on a tunnel interface.", req.Dst, req.Iface)
	}
	routes, err := s.Table.Routes()
	if err != nil {
		return failed("reading routes: %v", err)
	}
	for _, r := range routes {
		if r.Dst.String() != req.Dst || r.Iface != req.Iface {
			continue
		}
		if err := s.Journal.record(Deleted, r); err != nil {
			return failed("journaling %s: %v", r, err)
		}
		if err := s.Table.Delete(r); err != nil {
			s.Journal.forget(Deleted, r)
			return failed("deleting %s: %v", r, err)
		}
		return nil
	}
	return failed("There is no route %s on %s.", req.Dst, req.Iface)
}

func invalid(format string, a ...any) *Error {
	return &Error{CodeInvalid, fmt.Sprintf(format, a...)}
}

func refused(format string, a ...any) *Error {
	return &Error{CodeRefused, fmt.Sprintf(format, a...)}
}

func failed(format string, a ...any) *Error {
	return &Error{CodeFailed, fmt.Sprintf(format, a...)}
}
