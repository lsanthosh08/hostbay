// Package wol sends Wake-on-LAN magic packets, from this computer or through
// a connected SSH host on the target's LAN.
package wol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/admin"
)

// parseMAC reads six octets of one or two hex digits separated by ':' or
// '-'. wol.rs trimmed each octet, which let whitespace into the relay
// command line; this does not.
func parseMAC(s string) ([6]byte, error) {
	var mac [6]byte
	parts := strings.Split(strings.ReplaceAll(strings.TrimSpace(s), "-", ":"), ":")
	if len(parts) != 6 {
		return mac, errors.New("MAC must be 6 octets (aa:bb:cc:dd:ee:ff)")
	}
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 16, 8) // no sign, no underscores in base 16
		if err != nil || len(p) > 2 {
			return mac, fmt.Errorf("invalid MAC octet: %q", p)
		}
		mac[i] = byte(v)
	}
	return mac, nil
}

// magicPacket is 6 bytes of 0xFF and then the MAC 16 times.
func magicPacket(mac [6]byte) []byte {
	return append(bytes.Repeat([]byte{0xff}, 6), bytes.Repeat(mac[:], 16)...)
}

// Wake broadcasts the magic packet on this computer's LAN to ports 9 and 7,
// as wol.rs did. Go enables SO_BROADCAST on UDP sockets.
func Wake(mac string) error {
	return wakeTo(mac, "255.255.255.255:9", "255.255.255.255:7")
}

func wakeTo(mac string, addrs ...string) error {
	m, err := parseMAC(mac)
	if err != nil {
		return err
	}
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return fmt.Errorf("bind: %w", err)
	}
	defer conn.Close()
	pkt := magicPacket(m)
	for _, a := range addrs {
		ua, err := net.ResolveUDPAddr("udp4", a)
		if err == nil {
			_, err = conn.WriteTo(pkt, ua)
		}
		if err != nil {
			return fmt.Errorf("send %s: %w", a, err)
		}
	}
	return nil
}

// WakeVia runs a sender on a connected relay host that shares the target's
// LAN: wakeonlan, else etherwake, else a Perl broadcast to port 9. The MAC
// reaches the command only in canonical form (hex and colons). wol.rs called
// a failure success when it printed nothing to stderr, and said nothing
// useful when no tool was installed; both are errors here.
func WakeVia(ctx context.Context, c *ssh.Client, mac string) error {
	b, err := parseMAC(mac)
	if err != nil {
		return err
	}
	m := net.HardwareAddr(b[:]).String()
	perl := `my $mac = "` + m + `"; $mac =~ s/://g; my $t = pack("H*", $mac); my $p = (chr(0xff) x 6) . ($t x 16); ` +
		`socket(S, 2, 2, 0) or die; setsockopt(S, 1, 6, 1); my $a = pack("Sna4x8", 2, 9, "\xff\xff\xff\xff"); send(S, $p, 0, $a) or die "send: $!";`
	cmd := "if command -v wakeonlan >/dev/null 2>&1; then wakeonlan " + m + "; " +
		"elif command -v etherwake >/dev/null 2>&1; then sudo -n etherwake " + m + " 2>/dev/null || etherwake " + m + "; " +
		"elif command -v perl >/dev/null 2>&1; then perl -e '" + perl + "'; " +
		"else echo 'no wake tool on the relay host (tried wakeonlan, etherwake, perl)' >&2; exit 127; fi"
	r, err := admin.Exec(ctx, c, cmd)
	if err != nil {
		return err
	}
	if r.Code != nil && *r.Code == 0 {
		return nil
	}
	msg := strings.TrimSpace(r.Stderr + "\n" + r.Stdout)
	if msg == "" {
		msg = "the wake command failed"
	}
	return errors.New("relay wake failed: " + msg)
}
