package openvpn

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Event states.
const (
	Connecting = "connecting"
	Connected  = "connected"
	Exiting    = "exiting"   // OpenVPN is ending; Reason says why
	Failed     = "failed"    // the sign-in or the run failed; Reason says why
	Challenge  = "challenge" // the server wants a browser sign-in at URL first (CRV1)
)

// Refused is the Failed reason when the server refuses the user name and
// password, which callers tell apart from a run that failed on its own.
const Refused = "The VPN server refused the user name or password."

// lineMax is the longest line OpenVPN's management interface reads; a
// password that does not fit goes in as base64 lines.
const lineMax = 1024

// Event is what the management interface reported.
type Event struct {
	State  string
	Local  string // Connected: the tunnel's own address
	Reason string
	SID    string // Challenge: the session the browser sign-in answers
	URL    string // Challenge: the http or https sign-in address
}

// Manage speaks to OpenVPN's management interface on rw until it closes:
// it gives the interface's password (mgmtPass, Windows), turns state
// notifications on, releases the hold Build's configuration starts in, and
// answers the first request for the user name and password with user and
// pass; a second request is refused and ends the run, so a wrong password
// is sent once. A pass too long for one management line, such as a SAML
// response, goes in as base64 lines. Each change is passed to on. Neither
// credential is ever logged or put in an error.
func Manage(rw io.ReadWriter, user, pass, mgmtPass string, on func(Event)) error {
	return ManageOTP(rw, user, pass, mgmtPass, nil, on)
}

// ManageOTP is Manage for a profile that may ask for a one-time code
// (static-challenge): otp returns the code the user types, and the password
// goes out in OpenVPN's SCRV1 form. With no otp such a profile fails.
func ManageOTP(rw io.ReadWriter, user, pass, mgmtPass string, otp func() (string, error), on func(Event)) error {
	for _, s := range []string{user, pass, mgmtPass} {
		if strings.ContainsAny(s, "\r\n\x00") {
			return errors.New("a VPN credential has a line break")
		}
	}
	send := func(lines ...string) error {
		_, err := io.WriteString(rw, strings.Join(lines, "\n")+"\n")
		return err
	}
	if mgmtPass != "" {
		if err := send(mgmtPass); err != nil {
			return err
		}
	}
	if err := send("state on"); err != nil {
		return err
	}
	answered := false
	sc := bufio.NewScanner(rw)
	sc.Buffer(nil, 1<<20) // a CRV1 challenge carries the whole SAML request in its URL
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimRight(sc.Text(), "\r"), "ENTER PASSWORD:")
		var err error
		switch {
		case strings.HasPrefix(line, ">HOLD:"):
			err = send("hold release")
		case strings.HasPrefix(line, ">PASSWORD:Need 'Auth'") && strings.Contains(line, " SC:") && !answered:
			answered = true
			if otp == nil {
				on(Event{State: Failed, Reason: "The VPN asks for a one-time code Hostbay cannot ask for here."})
				err = send("signal SIGTERM")
				break
			}
			code, cerr := otp()
			if cerr != nil || strings.ContainsAny(code, "\r\n\x00") {
				on(Event{State: Failed, Reason: "The one-time code was not given."})
				err = send("signal SIGTERM")
				break
			}
			b64 := base64.StdEncoding
			err = send(fmt.Sprintf(`username "Auth" %s`, quote(user)),
				fmt.Sprintf(`password "Auth" %s`, quote("SCRV1:"+b64.EncodeToString([]byte(pass))+":"+b64.EncodeToString([]byte(code)))))
		case strings.HasPrefix(line, ">PASSWORD:Need 'Auth'") && !answered:
			answered = true
			err = send(fmt.Sprintf(`username "Auth" %s`, quote(user)))
			if err == nil {
				err = send(passwordLines(pass)...)
			}
		case strings.HasPrefix(line, ">PASSWORD:Need "):
			on(Event{State: Failed, Reason: "The VPN asked again for a sign-in Hostbay cannot give."})
			err = send("signal SIGTERM")
		case strings.HasPrefix(line, ">PASSWORD:Verification Failed") && strings.Contains(line, "['CRV1:"):
			on(challenge(line))
		case strings.HasPrefix(line, ">PASSWORD:Verification Failed"):
			on(Event{State: Failed, Reason: Refused})
		case strings.HasPrefix(line, ">FATAL:"):
			on(Event{State: Failed, Reason: "OpenVPN: " + strings.TrimPrefix(line, ">FATAL:")})
		case strings.HasPrefix(line, ">STATE:"):
			if e := stateEvent(strings.Split(strings.TrimPrefix(line, ">STATE:"), ",")); e.State != "" {
				on(e)
			}
		}
		if err != nil {
			return err
		}
	}
	return sc.Err()
}

// passwordLines is the management command giving pass: one quoted line, or,
// when that would not fit, version 6's base64 form ending in END.
func passwordLines(pass string) []string {
	line := `password "Auth" ` + quote(pass)
	if len(line) < lineMax {
		return []string{line}
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(pass))
	lines := []string{"version 6", `password "Auth"`}
	for len(b64) > lineMax {
		lines, b64 = append(lines, b64[:lineMax]), b64[lineMax:]
	}
	return append(lines, b64, "END")
}

// challenge reads a refusal carrying ['CRV1:flags:sid:user:url']. Only an
// http or https address is passed on, since it is opened in the browser.
func challenge(line string) Event {
	crv := line[strings.Index(line, "['CRV1:")+2:]
	crv = strings.TrimSuffix(crv, "']")
	f := strings.SplitN(crv, ":", 5)
	if len(f) == 5 && f[2] != "" {
		if u, err := url.Parse(f[4]); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
			return Event{State: Challenge, SID: f[2], URL: f[4]}
		}
	}
	return Event{State: Failed, Reason: "The VPN server asked for a browser sign-in at an address Hostbay will not open."}
}

// CRV1Password is the password answering a challenge for session sid with
// the identity provider's SAML response.
func CRV1Password(sid, samlResponse string) string {
	return "CRV1::" + sid + "::" + url.QueryEscape(samlResponse)
}

// stateEvent reads a state line's fields: time, state, detail, local
// address, remote address, ...
func stateEvent(f []string) Event {
	if len(f) < 2 {
		return Event{}
	}
	field := func(i int) string {
		if i < len(f) {
			return f[i]
		}
		return ""
	}
	switch f[1] {
	case "CONNECTED":
		return Event{State: Connected, Local: field(3)}
	case "EXITING":
		return Event{State: Exiting, Reason: field(2)}
	}
	return Event{State: Connecting, Reason: field(2)}
}

// quote is a management-interface string: backslash and double quote
// escaped inside double quotes.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
