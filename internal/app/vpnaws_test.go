package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hostbay/internal/netd"
	"hostbay/internal/store"
	"hostbay/internal/vpn/proto"
	"hostbay/internal/vpn/vpntest"
)

const (
	awsTestSAML = "PHNhbWxwOlJlc3BvbnNl+/=" // with characters the password must escape
	awsTestURL  = "https://idp.example.test/sso?SAMLRequest=abc"
)

// fakeAWS plays each OpenVPN run of an AWS sign-in on a management socket
// of its own: the placeholder sign-in gets a CRV1 challenge, any other
// connects, or with refuse set is refused. With mute set no run ever hears
// from the server, as when it cannot be reached.
type fakeAWS struct {
	t      *testing.T
	refuse bool
	mute   bool

	mu    sync.Mutex
	fatal bool // signed-in runs fail on their own, as a lost adapter does
	// helperFirst: signed-in runs are refused, and the helper reports the
	// process gone before OpenVPN's management interface closes.
	helperFirst bool
	f           *fakeTunnels
	runs        int
	starts      []netd.Msg
	pass        chan string   // each run's password, as OpenVPN reads it
	later       chan string   // a connected run's commands
	drop        chan struct{} // drops the connected run, as a network change does
}

func useAWS(t *testing.T, e *vpnEnv) (*fakeAWS, *fakeTunnels) {
	t.Helper()
	a := &fakeAWS{t: t, pass: make(chan string, 64), later: make(chan string, 64), drop: make(chan struct{}, 1)}
	f := useTunnels(e.svc)
	f.reply = a.reply
	a.f = f
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	oldAddr, oldLookup := awsSSOAddr, awsLookup
	awsSSOAddr = addr
	awsLookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("2001:db8::7")}, {IP: net.ParseIP("203.0.113.7")}}, nil
	}
	t.Cleanup(func() { awsSSOAddr, awsLookup = oldAddr, oldLookup })
	return a, f
}

func (a *fakeAWS) reply(m netd.Msg) (netd.Msg, error) {
	dir, err := os.MkdirTemp("/tmp", "aws")
	if err != nil {
		return netd.Msg{}, err
	}
	a.t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "m.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return netd.Msg{}, err
	}
	a.mu.Lock()
	a.runs++
	n := a.runs
	a.starts = append(a.starts, m)
	a.mu.Unlock()
	go func() {
		defer ln.Close()
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		sc := bufio.NewScanner(c)
		read := func() string { sc.Scan(); return sc.Text() }
		say := func(s string) { c.Write([]byte(s + "\r\n")) }
		if read() != "state on" {
			return
		}
		say(">HOLD:Waiting for hold release:0")
		read()
		say(">PASSWORD:Need 'Auth' username/password")
		read() // the user name
		pw := read()
		if pw == "version 6" {
			read() // password "Auth"
			var b64 strings.Builder
			for l := read(); l != "END"; l = read() {
				b64.WriteString(l)
			}
			b, _ := base64.StdEncoding.DecodeString(b64.String())
			pw = string(b)
		} else {
			pw, _ = strconv.Unquote(strings.TrimPrefix(pw, `password "Auth" `))
		}
		a.pass <- pw
		switch {
		case a.mute:
			for sc.Scan() { // OpenVPN keeps retrying until it is told to stop
				if sc.Text() == "signal SIGTERM" {
					return
				}
			}
		case pw == "ACS::35001":
			say(">PASSWORD:Verification Failed: 'Auth' ['CRV1:R:sid-" + strconv.Itoa(n) + ":Ti9B:" + awsTestURL + "']")
		case a.refuse:
			say(">PASSWORD:Verification Failed: 'Auth'")
		case a.failing():
			say(">FATAL:Cannot open TUN/TAP dev")
		case a.refusedHelperFirst():
			say(">PASSWORD:Verification Failed: 'Auth'")
			time.Sleep(100 * time.Millisecond) // the refusal has been read
			a.f.event(netd.Msg{Tunnel: m.Tunnel, State: netd.TunnelEnded, Reason: "OpenVPN stopped."})
		default:
			say(">STATE:1700000001,CONNECTED,SUCCESS,127.0.0.1,203.0.113.7,443,,")
			ended := make(chan struct{})
			defer close(ended)
			go func() {
				select {
				case <-a.drop:
					c.Close()
				case <-ended:
				}
			}()
			for sc.Scan() {
				a.later <- sc.Text()
				if sc.Text() == "signal SIGTERM" {
					return
				}
			}
		}
	}()
	return netd.Msg{Mgmt: sock}, nil
}

func (a *fakeAWS) failing() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fatal
}

func (a *fakeAWS) refusedHelperFirst() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.helperFirst
}

func (a *fakeAWS) started() []netd.Msg {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]netd.Msg{}, a.starts...)
}

func (a *fakeAWS) password(t *testing.T) string {
	t.Helper()
	select {
	case p := <-a.pass:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no password sent")
	}
	return ""
}

func saveAWS(t *testing.T, e *vpnEnv, randomHost bool) VPN {
	t.Helper()
	v, err := e.svc.SaveVPN(VPN{Name: "AWS", Type: "openvpn", Tier: store.VPNTierSystem, Gateway: "cvpn-endpoint-0.example.test", Port: 443,
		Auth: "browser", AutoReconnect: true, Routes: []string{"10.30.0.0/16"}})
	if err != nil {
		t.Fatal(err)
	}
	if randomHost {
		e.svc.st.Edit(func(d *store.Data) error { d.VPN(v.ID).RandomHost = true; return nil })
	}
	return v
}

// postSAML posts the identity provider's form to the sign-in listener.
func postSAML(t *testing.T) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+awsSSOAddr+"/", strings.NewReader(url.Values{"SAMLResponse": {awsTestSAML}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://idp.example.test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Error(err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("sign-in post: %s", resp.Status)
	}
}

// browser plays the default browser: it records each page opened and,
// after delay, signs in there unless sign is false.
func browser(t *testing.T, e *vpnEnv, sign bool, delay time.Duration) chan string {
	opened := make(chan string, 8)
	e.svc.openURL = func(u string) {
		opened <- u
		if sign {
			go func() { time.Sleep(delay); postSAML(t) }()
		}
	}
	return opened
}

func connectAsync(e *vpnEnv, id string) {
	go e.svc.ConnectVPN(context.Background(), id)
}

func waitStatus(t *testing.T, e *vpnEnv, id, status string) VPNState {
	t.Helper()
	var st VPNState
	waitFor(t, func() bool { st = stateOf(e.svc, id); return st.Status == status })
	return st
}

// One connect runs OpenVPN twice around the browser sign-in: the first run
// fetches the challenge, the second signs in with the SAML response, and
// both reach the one address the gateway resolved to.
func TestAWSSSOSignsInThroughTheBrowser(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	var looked string
	awsLookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
		looked = host
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.7")}}, nil
	}
	opened := browser(t, e, true, 0)
	v := saveAWS(t, e, true)
	connectAsync(e, v.ID)

	if p := a.password(t); p != "ACS::35001" {
		t.Fatalf("first password %q", p)
	}
	select {
	case u := <-opened:
		if u != awsTestURL {
			t.Fatalf("opened %q", u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the browser was not opened")
	}
	if p := a.password(t); p != "CRV1::sid-1::"+url.QueryEscape(awsTestSAML) {
		t.Fatalf("second password %q", p)
	}
	st := waitStatus(t, e, v.ID, VPNConnected)
	if st.Iface != loopback(t) {
		t.Fatalf("state %+v", st)
	}
	starts := a.started()
	if len(starts) != 2 {
		t.Fatalf("%d runs", len(starts))
	}
	for _, s := range starts {
		if !s.OpenVPN.AWS || s.OpenVPN.Remote != "203.0.113.7" || s.OpenVPN.Port != 443 {
			t.Fatalf("run %+v", s.OpenVPN)
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{24}\.cvpn-endpoint-0\.example\.test$`).MatchString(looked) {
		t.Fatalf("resolved %q", looked)
	}
	// The sign-in is over: the port is free again.
	ln, err := net.Listen("tcp", awsSSOAddr)
	if err != nil {
		t.Fatalf("the sign-in port is still held: %v", err)
	}
	ln.Close()
}

// The browser wait is not bound by OpenVPN's connect timeout.
func TestAWSSSOSlowSignInStillConnects(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	useAWS(t, e)
	old := openvpnConnectWait
	openvpnConnectWait = 300 * time.Millisecond
	t.Cleanup(func() { openvpnConnectWait = old })
	browser(t, e, true, time.Second)
	v := saveAWS(t, e, false)
	connectAsync(e, v.ID)
	waitStatus(t, e, v.ID, VPNSigningIn)
	waitStatus(t, e, v.ID, VPNConnected)
}

// Disconnect during the browser wait ends the connect, frees the port and
// opens nothing more.
func TestAWSSSODisconnectWhileSigningIn(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	opened := browser(t, e, false, 0)
	v := saveAWS(t, e, false)
	connectAsync(e, v.ID)
	st := waitStatus(t, e, v.ID, VPNSigningIn)
	if st.Need != proto.NeedBrowser || st.URL != awsTestURL {
		t.Fatalf("state %+v", st)
	}
	if err := e.svc.DisconnectVPN(v.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		ln, err := net.Listen("tcp", awsSSOAddr)
		if err == nil {
			ln.Close()
		}
		return err == nil
	})
	time.Sleep(100 * time.Millisecond)
	if st := stateOf(e.svc, v.ID); st.Status != VPNDisconnected || len(a.started()) != 1 || len(opened) != 1 {
		t.Fatalf("state %+v, %d runs, %d pages", st, len(a.started()), len(opened))
	}
	// The lock went with it: another sign-in may start.
	release, err := takeAWSSSOLock()
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// A gateway that does not resolve fails the connect before any run starts,
// and lets go of the sign-in lock.
func TestAWSSSOUnresolvedGatewayFails(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	awsLookup = func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("no such host") }
	v := saveAWS(t, e, false)
	_, err := e.svc.ConnectVPN(context.Background(), v.ID)
	if err == nil || !strings.Contains(err.Error(), "could not find its server") || len(a.started()) != 0 {
		t.Fatalf("err %v, %d runs", err, len(a.started()))
	}
	release, err := takeAWSSSOLock()
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// A first run that never reaches the server fails after openvpnConnectWait
// instead of staying "connecting", and lets go of the sign-in lock.
func TestAWSSSOUnreachableServerFails(t *testing.T) {
	old := openvpnConnectWait
	openvpnConnectWait = 200 * time.Millisecond
	t.Cleanup(func() { openvpnConnectWait = old })
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	a.mute = true
	opened := browser(t, e, true, 0)
	v := saveAWS(t, e, false)
	connectAsync(e, v.ID)
	st := waitStatus(t, e, v.ID, VPNFailed)
	if !strings.Contains(st.Reason, "did not connect") || len(opened) != 0 {
		t.Fatalf("reason %q, %d pages", st.Reason, len(opened))
	}
	release, err := takeAWSSSOLock()
	if err != nil {
		t.Fatal(err)
	}
	release()
}

// A refused response fails the connect with a reason that carries none of
// it, and on the user's own connect nothing retries.
func TestAWSSSORefusedSignInFails(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	a.refuse = true
	opened := browser(t, e, true, 0)
	v := saveAWS(t, e, false)
	connectAsync(e, v.ID)
	st := waitStatus(t, e, v.ID, VPNFailed)
	if !strings.Contains(st.Reason, "refused") || strings.Contains(st.Reason, "PHNhbW") || strings.Contains(st.Reason, "sid-") {
		t.Fatalf("reason %q", st.Reason)
	}
	time.Sleep(200 * time.Millisecond)
	if len(a.started()) != 2 || len(opened) != 1 {
		t.Fatalf("%d runs, %d pages", len(a.started()), len(opened))
	}
}

// browserSeq plays a browser that signs in on the nth page opened only
// when signs[n] is true.
func browserSeq(t *testing.T, e *vpnEnv, signs ...bool) func() int {
	var mu sync.Mutex
	n := 0
	e.svc.openURL = func(string) {
		mu.Lock()
		sign := n < len(signs) && signs[n]
		n++
		mu.Unlock()
		if sign {
			go postSAML(t)
		}
	}
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// fastRetry makes reconnects and sign-in waits quick.
func fastRetry(t *testing.T) {
	oMin, oMax, oWait := vpnRetryMin, vpnRetryMax, awsSSOWait
	vpnRetryMin, vpnRetryMax, awsSSOWait = 10*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { vpnRetryMin, vpnRetryMax, awsSSOWait = oMin, oMax, oWait })
}

// connected connects v through a completed sign-in.
func connected(t *testing.T, e *vpnEnv, v VPN) {
	t.Helper()
	connectAsync(e, v.ID)
	waitStatus(t, e, v.ID, VPNConnected)
}

// A dropped tunnel signs in again by itself: the browser opens with no
// click, and the tunnel comes back.
func TestAWSSSODropSignsInAgain(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	fastRetry(t)
	opened := browserSeq(t, e, true, true)
	v := saveAWS(t, e, false)
	connected(t, e, v)
	a.drop <- struct{}{}
	waitFor(t, func() bool { return opened() == 2 && stateOf(e.svc, v.ID).Status == VPNConnected })
	if n := len(a.started()); n != 4 {
		t.Fatalf("%d runs", n)
	}
}

// Three automatic sign-ins in a row that are not completed stop the
// retries, and the browser is not opened a fourth time.
func TestAWSSSOStopsAfterThreeFailedSignIns(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	fastRetry(t)
	opened := browserSeq(t, e, true)
	v := saveAWS(t, e, false)
	connected(t, e, v)
	a.drop <- struct{}{}
	st := waitStatus(t, e, v.ID, VPNFailed)
	if !strings.Contains(st.Reason, "3 sign-ins") {
		t.Fatalf("reason %q", st.Reason)
	}
	time.Sleep(200 * time.Millisecond)
	if n := opened(); n != 4 {
		t.Fatalf("browser opened %d times", n)
	}
}

// A completed sign-in starts the count over.
func TestAWSSSOCompletedSignInResetsTheCount(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	fastRetry(t)
	old := vpnStableUp
	vpnStableUp = 0 // each drop here comes at once; only the sign-in count is under test
	t.Cleanup(func() { vpnStableUp = old })
	opened := browserSeq(t, e, true, false, false, true, false, false, true)
	v := saveAWS(t, e, false)
	connected(t, e, v)
	a.drop <- struct{}{}
	waitFor(t, func() bool { return opened() == 4 && stateOf(e.svc, v.ID).Status == VPNConnected })
	a.drop <- struct{}{}
	waitFor(t, func() bool { return opened() == 7 && stateOf(e.svc, v.ID).Status == VPNConnected })
}

// While another sign-in waits in the browser, a drop opens no second
// window: the profile stops with the in-progress message.
func TestAWSSSODropWhileAnotherSignInWaits(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	fastRetry(t)
	opened := browserSeq(t, e, true, true)
	v := saveAWS(t, e, false)
	connected(t, e, v)
	release, err := takeAWSSSOLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	a.drop <- struct{}{}
	st := waitStatus(t, e, v.ID, VPNFailed)
	if !strings.Contains(st.Reason, "in progress") || strings.Contains(st.Reason, "tries") { // ended at once, not by the retry limit
		t.Fatalf("reason %q", st.Reason)
	}
	time.Sleep(200 * time.Millisecond)
	if opened() != 1 || len(a.started()) != 2 || stateOf(e.svc, v.ID).Status != VPNFailed {
		t.Fatalf("%d pages, %d runs, state %+v", opened(), len(a.started()), stateOf(e.svc, v.ID))
	}
}

// A sign-in port another app holds stops the retries with its message.
func TestAWSSSODropWhilePortHeld(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	fastRetry(t)
	opened := browserSeq(t, e, true, true)
	v := saveAWS(t, e, false)
	connected(t, e, v)
	ln, err := net.Listen("tcp", awsSSOAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	a.drop <- struct{}{}
	st := waitStatus(t, e, v.ID, VPNFailed)
	if !strings.Contains(st.Reason, "in use") {
		t.Fatalf("reason %q", st.Reason)
	}
	time.Sleep(200 * time.Millisecond)
	if opened() != 1 || len(a.started()) != 3 {
		t.Fatalf("%d pages, %d runs", opened(), len(a.started()))
	}
}

// On the user's own connect a sign-in not completed ends it: no retry, and
// the browser opens once.
func TestAWSSSOOwnConnectTimeoutDoesNotRetry(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	useAWS(t, e)
	fastRetry(t)
	opened := browserSeq(t, e)
	v := saveAWS(t, e, false)
	connectAsync(e, v.ID)
	st := waitStatus(t, e, v.ID, VPNFailed)
	if !strings.Contains(st.Reason, "not completed") {
		t.Fatalf("reason %q", st.Reason)
	}
	time.Sleep(200 * time.Millisecond)
	if opened() != 1 {
		t.Fatalf("browser opened %d times", opened())
	}
}

// A signed-in run that fails for another reason than the server refusing
// it is not a failed sign-in: reconnects go on to the limit every profile
// has, and the reason says nothing of sign-ins.
func TestAWSSSOOtherFailuresAreNotFailedSignIns(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	fastRetry(t)
	opened := browserSeq(t, e, true, true, true, true, true, true, true, true, true)
	v := saveAWS(t, e, false)
	connected(t, e, v)
	a.mu.Lock()
	a.fatal = true
	a.mu.Unlock()
	a.drop <- struct{}{}
	st := waitStatus(t, e, v.ID, VPNFailed)
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(st.Reason, "sign-ins") || opened() <= 1+awsSignInTries {
		t.Fatalf("reason %q, browser opened %d times", st.Reason, opened())
	}
}

// A refusal still counts when the helper reports OpenVPN gone before its
// management interface closes, and its reason is the one shown.
func TestAWSSSORefusalCountsWhenTheHelperEndsFirst(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	fastRetry(t)
	opened := browserSeq(t, e, true, true, true, true, true, true, true, true, true)
	v := saveAWS(t, e, false)
	connected(t, e, v)
	a.mu.Lock()
	a.helperFirst = true
	a.mu.Unlock()
	a.drop <- struct{}{}
	st := waitStatus(t, e, v.ID, VPNFailed)
	time.Sleep(200 * time.Millisecond)
	if !strings.Contains(st.Reason, "3 sign-ins") || !strings.Contains(st.Reason, "refused") || opened() != 1+awsSignInTries {
		t.Fatalf("reason %q, browser opened %d times", st.Reason, opened())
	}
}

// Sign-ins that each connect and drop at once still stop at the reconnect
// limit every profile has.
func TestAWSSSOQuickDropsStopAtTheRetryLimit(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	fastRetry(t)
	opened := browserSeq(t, e, true, true, true, true, true, true, true, true)
	v := saveAWS(t, e, false)
	connected(t, e, v)
	for i := 0; i < 6; i++ {
		waitStatus(t, e, v.ID, VPNConnected)
		a.drop <- struct{}{}
		waitFor(t, func() bool { return stateOf(e.svc, v.ID).Status != VPNConnected })
	}
	waitStatus(t, e, v.ID, VPNFailed)
	time.Sleep(200 * time.Millisecond)
	if opened() != 6 {
		t.Fatalf("browser opened %d times", opened())
	}
}

// With automatic reconnect off, a drop just ends.
func TestAWSSSODropWithoutAutoReconnect(t *testing.T) {
	e := newVPNEnv(t, vpntest.OK)
	a, _ := useAWS(t, e)
	fastRetry(t)
	opened := browserSeq(t, e, true, true)
	v := saveAWS(t, e, false)
	v.AutoReconnect = false
	if _, err := e.svc.SaveVPN(v); err != nil {
		t.Fatal(err)
	}
	connected(t, e, v)
	a.drop <- struct{}{}
	waitStatus(t, e, v.ID, VPNDropped)
	time.Sleep(200 * time.Millisecond)
	if opened() != 1 || stateOf(e.svc, v.ID).Status != VPNDropped {
		t.Fatalf("%d pages, state %+v", opened(), stateOf(e.svc, v.ID))
	}
}
