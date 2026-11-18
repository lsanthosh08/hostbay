// Package portowner names the process that listens on a local TCP port, so
// a "port in use" error can say which app to quit. The lookup is best
// effort: callers fall back to a message without a name.
package portowner

import (
	"encoding/binary"
	"strings"
)

// lsofName is the first process name in `lsof -Fpc` output: one field per
// line, the field's letter first ("p1234", "cAWS VPN Client", "f9"). It is
// "" when the output names no process.
func lsofName(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "c"); ok && name != "" {
			return name
		}
	}
	return ""
}

// tablePID is the owner of the first listener on port in a
// MIB_TCPTABLE_OWNER_PID, as GetExtendedTcpTable fills it: a uint32 row
// count, then rows of six uint32s (state, local address, local port, remote
// address, remote port, process ID), the ports in network byte order. It is
// 0 when no row has the port.
func tablePID(table []byte, port int) uint32 {
	if len(table) < 4 {
		return 0
	}
	n := binary.LittleEndian.Uint32(table)
	rows := table[4:]
	for i := uint32(0); i < n && len(rows) >= 24; i++ {
		// Any local address counts: 0.0.0.0:port holds the port too.
		if p := binary.LittleEndian.Uint32(rows[8:]); int(p&0xff)<<8|int(p>>8&0xff) == port {
			return binary.LittleEndian.Uint32(rows[20:])
		}
		rows = rows[24:]
	}
	return 0
}
