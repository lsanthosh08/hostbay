package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/netd"
	"hostbay/internal/store"
	"hostbay/internal/vpn"
	"hostbay/internal/vpn/proto"
)

var (
	// vpnRetryMin and vpnRetryMax bound the wait between reconnects (R18).
	vpnRetryMin = time.Second
	vpnRetryMax = time.Minute
	// vpnStableUp is how long a profile must stay up before a drop starts
	// the backoff over: one that drops at once (a password rejected after
	// the first connect) keeps backing off, so it cannot lock an account.
	vpnStableUp = 2 * time.Minute
	// vpnRetryTries is how many reconnects in a row may fail without the
	// profile ever staying up (vpnStableUp) before Hostbay stops: a refused
	// or changed password would otherwise be sent every minute until the
	// account locks.
	vpnRetryTries = 6
)

// vpnRun is one connected profile, across reconnects.
type vpnRun struct {
	stop chan struct{} // closed by DisconnectVPN
	att  *vpnAttempt   // the current helper
	by   string        // who connected it: "" the user, or an MCP token's principal (U14)
}

// vpnAttempt is one helper process.
type vpnAttempt struct {
	h          *vpn.Helper // nil while starting
	user, pass string      // the per-run SOCKS login (KTD2)
	port       int         // the SOCKS port, once connected
	end        chan proto.Msg
	exited     chan struct{} // closed once the helper has exited, or never started
	nodeAAD    []byte        // Tailscale: what the node's state is sealed to, as the profile was at start
	pinned     string        // OpenConnect: the gateway pin the start carried
	sys        systemRun     // a system-wide run, in place of a helper
	iface      string        // a system-wide run's tunnel interface, once up
}

// close ends the attempt's helper or system-wide run, without waiting.
func (a *vpnAttempt) close() {
	if a.h != nil {
		a.h.Close()
	}
	if a.sys != nil {
		a.sys.Close()
	}
}

// waitExited waits until the profile's latest helper has exited, so nothing
// it writes while closing lands after what the caller does next.
func (s *VPNService) waitExited(id string) {
	s.mu.Lock()
	c := s.exited[id]
	s.mu.Unlock()
	if c != nil {
		<-c
	}
}

// VPNStates lists every profile whose state is known; the rest are
// disconnected.
func (s *VPNService) VPNStates() []VPNState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []VPNState{}
	for _, st := range s.states {
		out = append(out, st)
	}
	return out
}

// ConnectedVPNs counts the profiles Hostbay connected, so quitting can ask
// first (R19).
func (s *VPNService) ConnectedVPNs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// active lists the profiles that are up or hold a run: one connecting,
// signing in or waiting to reconnect takes its routes too (R13).
func (s *VPNService) active() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id := range s.runs {
		ids = append(ids, id)
	}
	for id, st := range s.states {
		if st.Status == VPNConnected && s.runs[id] == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// set records and announces a state; call it with s.mu held so events go
// out in order.
func (s *VPNService) set(st VPNState) VPNState {
	s.states[st.ID] = st
	s.emit(VPNStateEvent, st)
	close(s.changed)
	s.changed = make(chan struct{})
	return st
}

// ConnectVPN connects the profile and waits until it is up, or failed; any
// failure is also shown as the profile's state. A profile already
// connecting is waited on, and one already up returns its state, so any
// number of callers share one helper. ctx bounds only the wait: the
// connection lives until DisconnectVPN, quit, or its own end.
func (s *VPNService) ConnectVPN(ctx context.Context, id string) (VPNState, error) {
	return s.connectAs(ctx, id, "")
}

// connectAs is ConnectVPN for by, who owns the run only if this call
// starts it.
func (s *VPNService) connectAs(ctx context.Context, id, by string) (VPNState, error) {
	var v *store.VPN // nil when unreadable: the run reports it
	if d, err := s.st.Data(); err == nil {
		v = d.VPN(id)
	}
	if v != nil && v.Tier == store.VPNTierExternal {
		// Its own app connects it; Hostbay only sees the tunnel (KTD7).
		s.mu.Lock()
		st := s.states[id]
		s.mu.Unlock()
		return st, apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s connects in its own app; open it there", v.Name)).
			With("vpnId", id).With("reason", "external")
	}
	system := v != nil && vpnTier(*v) == store.VPNTierSystem && s.checkConnect != nil
	if system {
		s.connecting.Lock()
		if err := s.conflict(id); err != nil {
			s.connecting.Unlock()
			return VPNState{ID: id}, err
		}
	}
	s.mu.Lock()
	if s.runs[id] == nil {
		r := &vpnRun{stop: make(chan struct{}), by: by}
		s.runs[id] = r
		s.set(VPNState{ID: id, Status: VPNConnecting})
		go s.run(id, r)
	}
	s.mu.Unlock()
	if system {
		s.connecting.Unlock()
	}
	for {
		s.mu.Lock()
		st, running, changed := s.states[id], s.runs[id] != nil, s.changed
		s.mu.Unlock()
		switch {
		case st.Status == VPNConnected:
			return st, nil
		case !running:
			if st.err != nil {
				return st, st.err
			}
			msg := "VPN: " + st.Reason
			if st.Reason == "" {
				msg = "The VPN profile was disconnected."
			}
			return st, apperr.New(apperr.VPNFailed, msg).With("vpnId", id).With("reason", st.Status)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return st, ctx.Err()
		}
	}
}

// conflict is checkConnect's refusal as the coded error connects return.
func (s *VPNService) conflict(id string) error {
	if err := s.checkConnect(id); err != nil {
		return apperr.New(apperr.VPNFailed, err.Error()).With("vpnId", id).With("reason", "conflict")
	}
	return nil
}

// refusedStart reports whether err is a start the profile itself refuses:
// a routing conflict, or settings no config may be built from. Retrying
// cannot fix either.
func refusedStart(err error) bool {
	var ae *apperr.Error
	var ne *netd.Error
	if errors.As(err, &ne) { // the helper's own refusal, a profile Build will not accept
		return ne.Code == netd.CodeRefused || ne.Code == netd.CodeInvalid
	}
	return errors.As(err, &ae) && ae.Code == apperr.VPNFailed && (ae.Details["reason"] == "conflict" || ae.Details["reason"] == "invalid")
}

// run brings the profile up and keeps it up: a dropped profile reconnects,
// with backoff, while its auto-reconnect setting is on (R18).
func (s *VPNService) run(id string, r *vpnRun) {
	wait, wasUp, tries := vpnRetryMin, false, 0
	for {
		end, err := s.attempt(id, r)
		var m proto.Msg
		var upFor time.Duration
		var sf *ssoFailure
		if errors.As(err, &sf) {
			m, err = sf.msg, nil // a coded failure, as a helper's would be
		} else if err == nil {
			select {
			case m = <-end:
				s.mu.Lock()
				att := r.att
				if st := s.states[id]; st.Status == VPNConnected {
					upFor = time.Since(time.UnixMilli(st.Since))
				}
				s.mu.Unlock()
				att.close() // one that reported its end but lingers must not hold up the next
			case <-r.stop:
				return
			}
		}
		up := m.State == proto.Dropped
		if up && upFor >= vpnStableUp {
			wait, tries = vpnRetryMin, 0
		}
		tries++
		wasUp = wasUp || up
		// A coded failure (an untrusted certificate, an openconnect too old)
		// or a refused start needs the user: retrying cannot fix it.
		needsUser := m.Code != "" || refusedStart(err)

		d, _ := s.st.Data()
		again := wasUp && !needsUser && d != nil && d.VPN(id) != nil && d.VPN(id).AutoReconnect
		gaveUp := again && tries >= vpnRetryTries
		if gaveUp {
			again = false
		}
		s.mu.Lock()
		if s.runs[id] != r { // disconnected
			s.mu.Unlock()
			return
		}
		st := VPNState{ID: id, Status: VPNFailed, Reason: m.Reason, err: err}
		switch {
		case err != nil:
			st.Reason = err.Error()
		case m.Code != "":
			st = codedFailure(id, r.att.pinned, m)
		}
		if wasUp && !needsUser {
			st.Status, st.err = VPNDropped, nil
		}
		if gaveUp {
			st.Status = VPNFailed
			st.Reason = fmt.Sprintf("Hostbay stopped reconnecting after %d tries. Check the sign-in, then connect again. %s", tries, st.Reason)
		}
		if !again {
			delete(s.runs, id)
		}
		s.set(st)
		s.mu.Unlock()
		if !again {
			return
		}
		select {
		case <-time.After(wait):
		case <-r.stop:
			return
		}
		wait = min(wait*2, vpnRetryMax)
		s.mu.Lock()
		if s.runs[id] != r {
			s.mu.Unlock()
			return
		}
		s.set(VPNState{ID: id, Status: VPNConnecting})
		s.mu.Unlock()
	}
}

// codedFailure is the failed state for a helper's coded failure, and the
// error ConnectVPN returns for it: its details carry the certificate the
// gateway presented and the one pinned, for the UI to ask (R21, AE8).
func codedFailure(id, pinned string, m proto.Msg) VPNState {
	st := VPNState{ID: id, Status: VPNFailed, Reason: m.Reason, Code: m.Code, ServerCert: m.ServerCert}
	if m.Code == proto.CodeNeedsSystemWide {
		st.Reason = "This VPN type needs system-wide mode on Windows, which runs through Hostbay's network helper."
	}
	e := apperr.New(apperr.VPNFailed, "VPN: "+st.Reason).With("vpnId", id).With("reason", m.Code)
	if m.ServerCert != "" {
		e.With("serverCert", m.ServerCert)
	}
	if m.Code == proto.CodeCertChanged && pinned != "" {
		e.With("pinned", pinned)
	}
	st.err = e
	return st
}

// attempt starts one helper for the profile as saved now. Its states update
// the profile's; the failed or dropped message that ends it comes back on
// the returned channel.
func (s *VPNService) attempt(id string, r *vpnRun) (<-chan proto.Msg, error) {
	a := &vpnAttempt{end: make(chan proto.Msg, 1), exited: make(chan struct{})}
	s.mu.Lock()
	prev := s.exited[id]
	s.exited[id], r.att = a.exited, a
	s.mu.Unlock()
	done, err := s.launch(id, r, a, prev)
	if err != nil {
		close(a.exited)
		return nil, err
	}
	go func() {
		<-done
		close(a.exited)
	}()
	return a.end, nil
}

// launch starts a's helper once the profile's previous one has exited, so
// two never run on one profile's state (a Tailscale node's), and the start
// carries what the last one wrote as it closed; a system-wide profile
// starts its system-wide run instead. It returns a channel closed once the
// helper or run has ended.
func (s *VPNService) launch(id string, r *vpnRun, a *vpnAttempt, prev <-chan struct{}) (<-chan struct{}, error) {
	if prev != nil {
		select {
		case <-prev:
		case <-r.stop:
			return nil, errors.New("the VPN profile was disconnected")
		}
	}
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	v := d.VPN(id)
	if v != nil && vpnTier(*v) == store.VPNTierSystem {
		// A reconnect is checked again: another profile may have taken its
		// routes while it was down (R13).
		if s.checkConnect != nil {
			if err := s.conflict(id); err != nil {
				return nil, err
			}
		}
		return s.launchSystem(id, r, a, v)
	}
	cookie := ""
	if signsInInWindow(v) {
		// The window comes before the helper: openconnect has no browser to sign in with.
		if cookie, err = s.ssoSignIn(id, r, a, v); err != nil {
			return nil, err
		}
	}
	start, err := s.startMsgFor(id, v)
	if err != nil {
		return nil, err
	}
	if cookie != "" {
		start.Secrets["cookie"] = cookie
	}
	path, err := s.helper()
	if err != nil {
		return nil, err
	}
	a.user, a.pass, a.pinned = start.SocksUser, start.SocksPass, v.PeerKey
	if v.Type == proto.TypeTailscale {
		a.nodeAAD = v.NodeStateAAD()
	}
	h, err := vpn.Start(context.Background(), path, start, func(m proto.Msg) { s.report(id, r, a, m) })
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	a.h = h
	stopped := s.runs[id] != r
	s.mu.Unlock()
	if stopped {
		h.Close()
	}
	return h.Done(), nil
}

// report applies one helper message to the profile's state.
func (s *VPNService) report(id string, r *vpnRun, a *vpnAttempt, m proto.Msg) {
	if m.T == proto.NodeState {
		// Even from a stale run: a node writes its state as it closes.
		s.keepNodeState(id, a, m.NodeState)
		return
	}
	if m.State == proto.Failed || m.State == proto.Dropped {
		select { // run sets the state, knowing whether it reconnects
		case a.end <- m:
		default: // only the first end counts
		}
		return
	}
	s.mu.Lock()
	if s.runs[id] != r || r.att != a {
		s.mu.Unlock()
		return
	}
	open := ""
	switch {
	case m.T == proto.Need:
		st := VPNState{ID: id, Status: VPNSigningIn, Need: m.Need}
		if m.Need == proto.NeedBrowser && webURL(m.URL) {
			st.URL, open = m.URL, m.URL
		}
		s.set(st)
	case m.State == proto.Connecting && s.states[id].Status != VPNConnecting:
		s.set(VPNState{ID: id, Status: VPNConnecting})
	case m.State == proto.Connected:
		a.port = m.SocksPort
		s.set(VPNState{ID: id, Status: VPNConnected, Address: m.Address, Routes: m.Routes, Iface: a.iface, Since: time.Now().UnixMilli()})
	}
	s.mu.Unlock()
	if open != "" {
		s.openURL(open)
	}
}

// webURL reports whether u is an http or https URL with a host: the only
// sign-in page a helper may have opened.
func webURL(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// keepNodeState seals a Tailscale node's state into this computer's local
// settings, which sync and backups never carry (KTD3). It is never logged.
// A state for a profile deleted, or moved to another tailnet, since the
// run began is dropped.
func (s *VPNService) keepNodeState(id string, a *vpnAttempt, state string) {
	if a.nodeAAD == nil || state == "" {
		return
	}
	d, err := s.st.Data()
	if err != nil {
		return
	}
	if v := d.VPN(id); v == nil || !bytes.Equal(v.NodeStateAAD(), a.nodeAAD) {
		return
	}
	sealed, err := s.vault.Seal([]byte(state), a.nodeAAD)
	if err != nil {
		return
	}
	s.st.EditLocal(func(l *store.Local) error {
		l.VPNNodeStates = setLocal(l.VPNNodeStates, id, sealed)
		return nil
	})
}

// AnswerVPN sends the one-time code a signing-in profile waits for (R20).
func (s *VPNService) AnswerVPN(id, code string) error {
	s.mu.Lock()
	r := s.runs[id]
	if r != nil && r.att != nil && s.states[id].Status == VPNSigningIn {
		if a, ok := r.att.sys.(interface{ Answer(string) error }); ok {
			s.mu.Unlock()
			return a.Answer(code) // a system-wide OpenVPN's management interface
		}
	}
	if r == nil || r.att == nil || r.att.h == nil || s.states[id].Status != VPNSigningIn {
		s.mu.Unlock()
		return errors.New("this VPN profile is not waiting for a code")
	}
	h := r.att.h
	s.mu.Unlock()
	return h.Answer(code) // the helper reports connecting again

}

// socks is where Hostbay dials through the connected app-scoped profile id:
// the helper's loopback SOCKS5 address and this run's login, looked up at
// each dial so a reconnect never leaves a stale port behind (KTD2). None of
// it may reach the UI, an event or a log.
func (s *VPNService) socks(id string) (addr, user, pass string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r == nil || r.att == nil || s.states[id].Status != VPNConnected {
		return "", "", "", apperr.New(apperr.VPNDown, "This VPN profile is not connected").With("vpnId", id)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(r.att.port)), r.att.user, r.att.pass, nil
}

// connected reports whether profile id's run is up.
func (s *VPNService) connected(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id] != nil && s.states[id].Status == VPNConnected
}

// systemIface is the tunnel interface of system-wide profile id's run.
func (s *VPNService) systemIface(id, name string) (string, error) {
	s.mu.Lock()
	st, running := s.states[id], s.runs[id] != nil
	s.mu.Unlock()
	switch {
	case !running || st.Status != VPNConnected:
		return "", apperr.New(apperr.VPNDown, fmt.Sprintf("VPN %s is not connected; connect it, then try again", name)).With("vpnId", id)
	case st.Iface == "":
		return "", apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s is connected, but Hostbay cannot tell which interface is its tunnel", name)).
			With("vpnId", id).With("reason", "iface")
	}
	return st.Iface, nil
}

// DisconnectVPN stops the profile's helper. Disconnecting a profile that is
// not connected clears a failed or dropped state.
func (s *VPNService) DisconnectVPN(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop(id)
	return nil
}

// disconnectAs stops the profile only if by connected it, checked under the
// same lock, so a run the user started meanwhile is never stopped.
func (s *VPNService) disconnectAs(id, by string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runs[id]; r == nil || r.by != by {
		return errors.New("this VPN profile is not connected by this client")
	}
	s.stop(id)
	return nil
}

// connectedBy is who connected the profile's run, if it has one.
func (s *VPNService) connectedBy(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runs[id]; r != nil {
		return r.by, true
	}
	if by, ok := s.extBy[id]; ok && s.states[id].Status == VPNConnected {
		return by, true
	}
	return "", false
}

// stop ends the profile's run; s.mu is held.
func (s *VPNService) stop(id string) {
	r := s.runs[id]
	if r != nil {
		delete(s.runs, id)
		close(r.stop)
		if r.att != nil {
			r.att.close()
		}
	}
	// An external profile's state is its tunnel's, which this cannot end;
	// a system-wide run's tunnel ends with it.
	if st, known := s.states[id]; known && (st.Iface == "" || r != nil) && st.Status != VPNDisconnected {
		s.set(VPNState{ID: id, Status: VPNDisconnected})
	}
}

// ServiceShutdown disconnects every profile when the app quits (R19).
func (s *VPNService) ServiceShutdown() error {
	if s.stopDetect != nil {
		s.stopDetect()
	}
	s.mu.Lock()
	var ids []string
	for id := range s.runs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.DisconnectVPN(id)
	}
	// A closing Tailscale node writes its keys; quitting first loses them.
	for _, id := range ids {
		s.waitExited(id)
	}
	return nil
}
