package agentexec

import (
	"bytes"
	"strconv"
	"strings"
)

// Shell is the family of a terminal's shell, which decides its envelope.
type Shell string

const (
	ShellPOSIX      Shell = "posix"
	ShellPowerShell Shell = "powershell"
	ShellNone       Shell = "" // Telnet, serial, raw TCP, fish, cmd, network devices: confirm-only, no envelope
)

// markerTag names Hostbay's completion marker, an OSC sequence the
// terminal does not show: ESC ] 777 ; hostbay-done ; <nonce> ; <status> BEL.
const markerTag = "hostbay-done"

func marker(nonce string) []byte { return []byte("\x1b]777;" + markerTag + ";" + nonce + ";") }

// quote single-quotes s for a POSIX shell.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Envelope is what Go writes for the approved bytes cmd (KTD34). A POSIX
// shell evals them, single-quoted by quote, then prints the marker with
// the exit status; PowerShell runs them in a script block. The marker's
// parts are separate arguments, so the echoed line never holds it whole.
// A leading space keeps the line out of shell history where the shell
// honours that. A shell with no envelope gets the bytes as they are.
func Envelope(sh Shell, cmd, nonce string) []byte {
	switch sh {
	case ShellPOSIX:
		return []byte(" eval " + quote(cmd) + `; printf '\033]777;%s;%s;%d\007' '` + markerTag + `' '` + nonce + `' "$?"` + "\r")
	case ShellPowerShell:
		return []byte(" & { " + cmd + " }; Write-Host -NoNewline ([char]27 + ']777;' + '" + markerTag + "' + ';' + '" + nonce +
			"' + ';' + [int](-not $?) + [char]7)\r")
	}
	return []byte(cmd + "\r")
}

// scanner finds one job's marker in its output, across chunk edges.
type scanner struct {
	want []byte
	tail []byte
}

func newScanner(nonce string) *scanner { return &scanner{want: marker(nonce)} }

// feed looks at the next chunk and returns the exit status once the whole
// marker has arrived.
func (s *scanner) feed(p []byte) (int, bool) {
	buf := append(s.tail, p...)
	if code, ok := s.find(buf); ok {
		return code, true
	}
	keep := len(s.want) + 12 // a marker cut at the edge: its start and a status
	if len(buf) > keep {
		buf = buf[len(buf)-keep:]
	}
	s.tail = append(s.tail[:0], buf...)
	return 0, false
}

// find returns the status of the last complete marker in b.
func (s *scanner) find(b []byte) (int, bool) {
	for i := bytes.LastIndex(b, s.want); i >= 0; i = bytes.LastIndex(b[:i], s.want) {
		rest := b[i+len(s.want):]
		end := bytes.IndexByte(rest, '\a')
		if end < 1 || end > 10 {
			continue
		}
		if code, err := strconv.Atoi(string(rest[:end])); err == nil {
			return code, true
		}
	}
	return 0, false
}
