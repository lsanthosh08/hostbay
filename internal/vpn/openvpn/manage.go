package openvpn

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Event states.
const (
	Connecting = "connecting"
	Connected  = "connected"
	Exiting    = "exiting" // OpenVPN is ending; Reason says why
	Failed     = "failed"  // the sign-in or the run failed; Reason says why
)

// Event is what the management interface reported.
type Event struct {
	State  string
	Local  string // Connected: the tunnel's own address
	Reason string
}

// Manage speaks to OpenVPN's management interface on rw until it closes:
// it gives the interface's password (mgmtPass, Windows), turns state
// notifications on, releases the hold Build's configuration starts in, and
// answers the first request for the user name and password with user and
// pass; a second request is refused and ends the run, so a wrong password
// is sent once. Each change is passed to on. Neither credential is ever
// logged or put in an error.
func Manage(rw io.ReadWriter, user, pass, mgmtPass string, on func(Event)) error {
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
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimRight(sc.Text(), "\r"), "ENTER PASSWORD:")
		var err error
		switch {
		case strings.HasPrefix(line, ">HOLD:"):
			err = send("hold release")
		case strings.HasPrefix(line, ">PASSWORD:Need 'Auth'") && !answered:
			answered = true
			err = send(fmt.Sprintf(`username "Auth" %s`, quote(user)), fmt.Sprintf(`password "Auth" %s`, quote(pass)))
		case strings.HasPrefix(line, ">PASSWORD:Need "):
			on(Event{State: Failed, Reason: "The VPN asked again for a sign-in Hostbay cannot give."})
			err = send("signal SIGTERM")
		case strings.HasPrefix(line, ">PASSWORD:Verification Failed"):
			on(Event{State: Failed, Reason: "The VPN server refused the user name or password."})
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
