package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/store"
	"hostbay/internal/vpn/proto"
)

// A gateway whose AnyConnect SAML sign-in runs only in an embedded browser
// cannot be signed in to by openconnect's command line, which has no browser
// of its own. hostbay-sso (cmd/hostbay-sso) is that browser: it shows the
// sign-in page in a window and returns the session cookie, which the VPN
// helper then hands to openconnect in place of a sign-in.

// ssoGroupsWait bounds asking a gateway for its groups.
const ssoGroupsWait = 20 * time.Second

// ssoOut is hostbay-sso's one line of output.
type ssoOut struct {
	Cookie  string   `json:"cookie"`
	Pin     string   `json:"pin"`
	Groups  []string `json:"groups"`
	Default string   `json:"default"`
	Error   string   `json:"error"`
	Code    string   `json:"code"`
}

// ssoFailure is a sign-in that failed with a coded reason the UI acts on,
// such as a gateway certificate the system does not trust (R21).
type ssoFailure struct{ msg proto.Msg }

func (e *ssoFailure) Error() string { return e.msg.Reason }

// ssoHelperPath finds hostbay-sso beside the running executable, as the
// other helpers are found. It is "" where there is none: only macOS builds
// it, and a build made without openconnect omits it.
func ssoHelperPath() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return ""
	}
	p := filepath.Join(filepath.Dir(exe), "hostbay-sso")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// signsInInWindow reports whether v signs in through hostbay-sso: an
// AnyConnect profile that signs in at a browser, on a build that has it.
func signsInInWindow(v *store.VPN) bool {
	return v != nil && v.Type == proto.TypeAnyConnect && v.Auth == "browser" && ssoHelperPath() != ""
}

// runSSO runs hostbay-sso with args until it ends or ctx does.
func runSSO(ctx context.Context, args ...string) (ssoOut, error) {
	var o ssoOut
	cmd := exec.CommandContext(ctx, ssoHelperPath(), args...)
	var out bytes.Buffer
	cmd.Stdout = &out // the cookie is only ever here, never an argument, a file or a log
	err := cmd.Run()
	line := strings.TrimSpace(out.String())
	if i := strings.LastIndexByte(line, '\n'); i >= 0 {
		line = line[i+1:]
	}
	if line != "" {
		if jerr := json.Unmarshal([]byte(line), &o); jerr != nil {
			o = ssoOut{}
		}
	}
	if ctx.Err() != nil {
		return o, errors.New("Sign-in was cancelled.")
	}
	if err != nil && o.Error == "" {
		o.Error = "The VPN sign-in window ended without signing in."
	}
	return o, nil
}

// ssoSignIn opens the sign-in window for profile id and returns the
// gateway's session cookie. Closing the window, or disconnecting the
// profile, ends it.
func (s *VPNService) ssoSignIn(id string, r *vpnRun, a *vpnAttempt, v *store.VPN) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-r.stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	a.pinned = v.PeerKey
	s.mu.Lock()
	if s.runs[id] == r {
		s.set(VPNState{ID: id, Status: VPNSigningIn, Need: proto.NeedBrowser})
	}
	s.mu.Unlock()

	args := []string{"--gateway", openConnectGateway(v.Gateway, v.Port)}
	if v.AuthGroup != "" {
		args = append(args, "--group", v.AuthGroup)
	}
	if v.User != "" {
		args = append(args, "--user", v.User)
	}
	if v.PeerKey != "" {
		args = append(args, "--pin", v.PeerKey)
	}
	o, err := runSSO(ctx, args...)
	if err != nil {
		return "", err
	}
	switch {
	case o.Cookie != "":
		return o.Cookie, nil
	case o.Code == proto.CodeCertUnknown || o.Code == proto.CodeCertChanged:
		return "", &ssoFailure{proto.Msg{T: proto.State, State: proto.Failed, Code: o.Code, ServerCert: o.Pin, Reason: o.Error}}
	}
	return "", errors.New(o.Error)
}

// VPNGroups asks a gateway which sign-in groups it offers, for the profile
// form's group list. It signs in to nothing. An empty list means the gateway
// offers none to choose from, or this build cannot ask.
func (s *VPNService) VPNGroups(gateway string, port int) ([]string, error) {
	if ssoHelperPath() == "" {
		return []string{}, nil
	}
	if gateway == "" || store.BadVPNText(gateway, true) || strings.HasPrefix(gateway, "-") {
		return nil, apperr.New(apperr.VPNFailed, "Enter the gateway first.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), ssoGroupsWait)
	defer cancel()
	o, err := runSSO(ctx, "--gateway", openConnectGateway(gateway, port), "--list-groups")
	if err != nil {
		return nil, errors.New("The gateway did not answer.")
	}
	if o.Error != "" {
		return nil, errors.New(o.Error)
	}
	if o.Groups == nil {
		return []string{}, nil
	}
	return o.Groups, nil
}
