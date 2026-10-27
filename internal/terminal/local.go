package terminal

import (
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

// LocalShell is one shell a local terminal can start.
type LocalShell struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// terminalEnv sets TERM for xterm.js and, when the app was started without
// a locale (as from the macOS Finder), a UTF-8 one so shells take UTF-8 input.
func terminalEnv(env []string) []string {
	out := make([]string, 0, len(env)+2)
	locale := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "TERM=") {
			continue
		}
		for _, k := range []string{"LANG=", "LC_ALL=", "LC_CTYPE="} {
			locale = locale || strings.HasPrefix(kv, k) && len(kv) > len(k)
		}
		out = append(out, kv)
	}
	if !locale {
		out = append(out, "LANG=en_US.UTF-8")
	}
	return append(out, "TERM=xterm-256color")
}

// parseWSLList reads the distribution names that `wsl.exe -l -q` prints.
// wsl.exe writes UTF-16LE, usually with a BOM, one name per line.
func parseWSLList(b []byte) []string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	var names []string
	for _, line := range strings.Split(string(utf16.Decode(u)), "\n") {
		if name := strings.Trim(line, "\ufeff\r\x00 \t"); name != "" {
			names = append(names, name)
		}
	}
	return names
}
