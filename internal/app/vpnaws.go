package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/netd"
	"hostbay/internal/store"
	"hostbay/internal/vpn/openvpn"
	"hostbay/internal/vpn/proto"
)

// An AWS Client VPN SSO connect runs OpenVPN twice. The first run signs in
// with AWS's fixed placeholder, and the server refuses it with a CRV1
// challenge naming the browser sign-in. The browser posts the identity
// provider's SAML response to the sign-in listener (awssso.go), and the
// second run signs in with it. Both runs must reach the server that issued
// the challenge, so the gateway is resolved once per connect.

// The first run's sign-in: AWS's client sends these to ask for the
// challenge, naming the port the browser will post to.
const (
	awsUser          = "N/A"
	awsChallengePass = "ACS::35001"
)

// awsResolveWait bounds the gateway lookup.
const awsResolveWait = 10 * time.Second

// awsLookup resolves the gateway; tests replace it.
var awsLookup = net.DefaultResolver.LookupIPAddr

// awsRun is one AWS SSO connect: the challenge run, the browser wait and
// the signed-in run, as one systemRun.
type awsRun struct {
	rep     func(proto.Msg)
	release func() // the sign-in lock

	mu  sync.Mutex
	cur *netdTunnel // the OpenVPN run now going, if any
	// signInFailed: it ended because the browser sign-in was not completed
	// or the server refused its response, which counts toward the limit on
	// automatic sign-ins.
	signInFailed bool
	// needsUser: it ended because the sign-in port is held by another app;
	// retrying cannot help until the user frees it.
	needsUser bool

	closing, done chan struct{}
	closeOnce     sync.Once
	endOnce       sync.Once
}

// startAWS starts an AWS SSO connect for v, whose OpenVPN profile is p.
// It holds the sign-in lock until the tunnel is up or the connect ends.
func (s *VPNService) startAWS(c tunnelClient, v *store.VPN, p *openvpn.Profile, up func(string, []string), rep func(proto.Msg)) (systemRun, error) {
	release, err := takeAWSSSOLock()
	if err != nil {
		return nil, withVPNID(err, v.ID)
	}
	addr, err := resolveAWS(v)
	if err != nil {
		release()
		return nil, apperr.New(apperr.VPNFailed, fmt.Sprintf("VPN %s could not find its server: %v", v.Name, err)).With("vpnId", v.ID)
	}
	p.AWS, p.Remote, p.UserPass = true, addr, false
	r := &awsRun{rep: rep, release: release, closing: make(chan struct{}), done: make(chan struct{})}
	// Set from manage's goroutine, read once it has returned.
	var chal struct{ sid, url string }
	var firstEnd proto.Msg
	first, err := startNetd(c, netd.Msg{T: netd.OpenVPNStart, Tunnel: rand.Text()[:16], OpenVPN: p}, up,
		func(m proto.Msg) {
			if m.State == proto.Failed || m.State == proto.Dropped {
				firstEnd = m // the run's end, reported only when it brought no challenge
				return
			}
			rep(m)
		}, awsUser, awsChallengePass,
		func(sid, url string) { chal.sid, chal.url = sid, url })
	if err != nil {
		release()
		return nil, err
	}
	r.cur = first
	go func() {
		// The helper's ended event can come before the management interface
		// has delivered the challenge, so wait for both.
		<-first.Done()
		<-first.managed
		if chal.sid == "" {
			r.end(firstEnd.Reason, false)
			return
		}
		s.awsSignIn(c, r, p, chal.sid, chal.url, up)
	}()
	return r, nil
}

// awsSignIn waits for the browser sign-in at url, then starts the second
// run with its response.
func (s *VPNService) awsSignIn(c tunnelClient, r *awsRun, p *openvpn.Profile, sid, url string, up func(string, []string)) {
	select {
	case <-r.closing:
		r.end("", false)
		return
	default:
	}
	l, err := listenAWSSSO()
	if err != nil {
		r.mu.Lock()
		r.needsUser = awsReason(err) == awsSSOReasonPort
		r.mu.Unlock()
		r.end(err.Error(), false)
		return
	}
	r.rep(proto.Msg{T: proto.Need, Need: proto.NeedBrowser, URL: url})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-r.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	resp, err := l.wait(ctx)
	cancel()
	if err != nil {
		r.end(err.Error(), true)
		return
	}
	r.rep(proto.Msg{T: proto.State, State: proto.Connecting})
	second, err := startNetd(c, netd.Msg{T: netd.OpenVPNStart, Tunnel: rand.Text()[:16], OpenVPN: p},
		func(iface string, addrs []string) { r.release(); up(iface, addrs) },
		func(m proto.Msg) {
			if m.State == proto.Failed { // ended before it was up: the server refused the sign-in
				r.mu.Lock()
				r.signInFailed = true
				r.mu.Unlock()
			}
			r.rep(m)
		}, awsUser, openvpn.CRV1Password(sid, resp), nil)
	if err != nil {
		r.end(err.Error(), false)
		return
	}
	r.mu.Lock()
	r.cur = second
	r.mu.Unlock()
	select {
	case <-r.closing:
		second.Close()
	default:
	}
	<-second.Done()
	r.endOnce.Do(func() { r.release(); close(r.done) }) // the run reported its own end
}

// end reports the connect's end once: failed, since no run of it came up,
// unless it was closed. signIn marks a sign-in that did not complete.
func (r *awsRun) end(reason string, signIn bool) {
	r.endOnce.Do(func() {
		r.release()
		select {
		case <-r.closing:
		default:
			r.mu.Lock()
			r.signInFailed = r.signInFailed || signIn
			r.mu.Unlock()
			ended(r.rep, false, reason)
		}
		close(r.done)
	})
}

// Close ends the connect at whatever stage it is in.
func (r *awsRun) Close() {
	r.closeOnce.Do(func() {
		close(r.closing)
		r.mu.Lock()
		cur := r.cur
		r.mu.Unlock()
		if cur != nil {
			cur.Close()
		}
	})
}

func (r *awsRun) Done() <-chan struct{} { return r.done }

// outcome says how the connect ended, for the reconnect rule.
func (r *awsRun) outcome() (signInFailed, needsUser bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.signInFailed, r.needsUser
}

// resolveAWS is the server address both runs of a connect use: the
// gateway, or with remote-random-hostname a random subdomain of it as
// AWS's client picks, resolved once.
func resolveAWS(v *store.VPN) (string, error) {
	host := v.Gateway
	if v.RandomHost {
		b := make([]byte, 12)
		rand.Read(b)
		host = hex.EncodeToString(b) + "." + host
	}
	ctx, cancel := context.WithTimeout(context.Background(), awsResolveWait)
	defer cancel()
	addrs, err := awsLookup(ctx, host)
	if err != nil {
		return "", err
	}
	for _, a := range addrs { // AWS endpoints answer on IPv4
		if a.IP.To4() != nil {
			return a.IP.String(), nil
		}
	}
	if len(addrs) > 0 {
		return addrs[0].IP.String(), nil
	}
	return "", errors.New("no address")
}

// awsReason is an AWS sign-in error's reason detail.
func awsReason(err error) string {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Details["reason"]
	}
	return ""
}

// withVPNID adds the profile to an apperr.Error, as the UI looks it up.
func withVPNID(err error, id string) error {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.With("vpnId", id)
	}
	return err
}
