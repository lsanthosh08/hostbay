package terminal

import "net"

// OpenTCP is a raw TCP terminal on conn (R48): bytes pass both ways as
// they are, with no Telnet negotiation, and typed text also shows locally
// when echo is set. The caller dials conn through the host's jump host or
// proxy.
func OpenTCP(conn net.Conn, echo bool) Transport {
	return newStreamTerm(conn, echo, "connection closed by the host")
}
