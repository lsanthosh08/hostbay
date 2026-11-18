package app

import (
	"bufio"
	"context"
	"encoding/base64"
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
// of its own: odd runs refuse with a CRV1 challenge, even runs connect,
// or with refuse set are refused.
type fakeAWS struct {
	t      *testing.T
	refuse bool

	mu     sync.Mutex
	runs   int
	starts []netd.Msg
	pass   chan string // each run's password, as OpenVPN reads it
	later  chan string // a connected run's commands
}

func useAWS(t *testing.T, e *vpnEnv) (*fakeAWS, *fakeTunnels) {
	t.Helper()
	a := &fakeAWS{t: t, pass: make(chan string, 16), later: make(chan string, 16)}
	f := useTunnels(e.svc)
	f.reply = a.reply
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
		case n%2 == 1:
			say(">PASSWORD:Verification Failed: 'Auth' ['CRV1:R:sid-" + strconv.Itoa(n) + ":Ti9B:" + awsTestURL + "']")
		case a.refuse:
			say(">PASSWORD:Verification Failed: 'Auth'")
		default:
			say(">STATE:1700000001,CONNECTED,SUCCESS,127.0.0.1,203.0.113.7,443,,")
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
