package remotedesktop

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"hostbay/internal/apperr"
)

// The SPICE link exchange (spice-protocol's spice/protocol.h). Each channel
// is its own connection and links on its own: the client sends a link
// message, the server replies with its caps and an RSA-1024 link key, and
// the client sends its ticket, the password RSA-OAEP (SHA-1) encrypted
// under that key. Every field is little-endian.
const (
	spiceChannelMain   = 1
	spiceCapAuthSelect = 0 // common cap: the client names its auth mechanism before the ticket
	spiceAuthSpice     = 1 // SPICE_COMMON_CAP_AUTH_SPICE, the ticket mechanism
	spicePubKeyLen     = 162
	spiceTicketLen     = 128
	spiceMaxPassword   = 60   // SPICE_MAX_PASSWORD_LENGTH
	spiceMaxLink       = 4096 // far above any real link message or reply
	spiceErrPermission = 7
)

var spiceLinkErrors = map[uint32]string{
	1: "an error", 2: "an invalid magic", 3: "invalid data", 4: "a version mismatch",
	5: "that the channel needs TLS", 6: "that the channel needs a plain port",
	7: "permission denied", 8: "a bad connection ID", 9: "that the channel is not available",
}

func spiceLinkError(code uint32) error {
	name, ok := spiceLinkErrors[code]
	if !ok {
		name = fmt.Sprintf("error %d", code)
	}
	return fmt.Errorf("the SPICE server refused the link with %s", name)
}

// SpiceMainLink is the main channel's link message exactly as the vendored
// spice-html5 0.3.0 sends it: auth selection and mini headers, and agent
// tokens. Go links the main channel with it before the page is there, so
// the page's own must match it byte for byte.
var SpiceMainLink = spiceLink(spiceChannelMain, 0, 1<<spiceCapAuthSelect|1<<3, 1<<2)

func spiceLink(typ, id uint8, common, channel uint32) []byte {
	b := append([]byte("REDQ"), 2, 0, 0, 0, 2, 0, 0, 0, 26, 0, 0, 0) // version 2.2, 26-byte body
	b = append(b, 0, 0, 0, 0, typ, id)                               // connection ID 0
	for _, v := range []uint32{1, 1, 18, common, channel} {          // cap counts, caps offset, caps
		b = binary.LittleEndian.AppendUint32(b, v)
	}
	return b
}

// SpiceChannel is the channel type and ID a link message asks for.
func SpiceChannel(link []byte) (typ, id uint8) { return link[20], link[21] }

// readSpiceLink reads a link header and its body, as both a link message
// and a link reply are framed.
func readSpiceLink(r io.Reader) ([]byte, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	if string(b[:4]) != "REDQ" {
		return nil, fmt.Errorf("not a SPICE link (it sent %q)", b[:4])
	}
	if major := binary.LittleEndian.Uint32(b[4:]); major != 2 {
		return nil, fmt.Errorf("SPICE protocol %d; Hostbay speaks 2", major)
	}
	n := binary.LittleEndian.Uint32(b[12:])
	if n < 18 || n > spiceMaxLink {
		return nil, fmt.Errorf("a SPICE link of %d bytes", n)
	}
	b = append(b, make([]byte, n)...)
	_, err := io.ReadFull(r, b[16:])
	return b, err
}

// authSelection reports whether a link message names its auth mechanism
// before the ticket; spice-server goes by the client's caps alone.
func authSelection(link []byte) bool {
	body := link[16:]
	if len(body) < 18 || binary.LittleEndian.Uint32(body[6:]) == 0 {
		return false
	}
	off := int(binary.LittleEndian.Uint32(body[14:]))
	return off+4 <= len(body) && binary.LittleEndian.Uint32(body[off:])&(1<<spiceCapAuthSelect) != 0
}

// SpiceSignIn links one SPICE channel on conn with the link message link
// and signs in with password: Go seals the ticket under the server's link
// key, so the password never leaves Go. With cfg the whole connection is
// TLS first, as on a SPICE tls-port, and a server that does not speak TLS
// is refused before anything is sent. It returns the connection to relay
// and the server's link reply. A refused password is SecretNeeded. The
// caller closes conn on error.
func SpiceSignIn(conn net.Conn, link []byte, password string, cfg *tls.Config) (_ net.Conn, _ []byte, err error) {
	if len(password) > spiceMaxPassword {
		return nil, nil, fmt.Errorf("SPICE passwords are at most %d bytes", spiceMaxPassword)
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer bound(conn, &err)()
	if cfg != nil {
		tc := tls.Client(conn, cfg)
		if err := tc.Handshake(); err != nil {
			return nil, nil, err // a *tls.CertificateVerificationError reaches tlstrust as is
		}
		conn = tc
	}
	if _, err := conn.Write(link); err != nil {
		return nil, nil, err
	}
	reply, err := readSpiceLink(conn)
	if err != nil {
		return nil, nil, fmt.Errorf("the SPICE server did not answer the link: %w", err)
	}
	body := reply[16:]
	if code := binary.LittleEndian.Uint32(body); code != 0 {
		return nil, nil, spiceLinkError(code)
	}
	if len(body) < 4+spicePubKeyLen {
		return nil, nil, errors.New("the SPICE server sent a short link reply")
	}
	k, err := x509.ParsePKIXPublicKey(body[4 : 4+spicePubKeyLen])
	pub, ok := k.(*rsa.PublicKey)
	if err != nil || !ok || pub.Size() != spiceTicketLen {
		return nil, nil, errors.New("the SPICE server sent an unusable link key")
	}
	ticket, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, append([]byte(password), 0), nil)
	if err != nil {
		return nil, nil, err
	}
	if authSelection(link) {
		ticket = append(binary.LittleEndian.AppendUint32(nil, spiceAuthSpice), ticket...)
	}
	if _, err := conn.Write(ticket); err != nil {
		return nil, nil, err
	}
	var r [4]byte
	if _, err := io.ReadFull(conn, r[:]); err != nil {
		return nil, nil, fmt.Errorf("the SPICE server did not answer the password: %w", err)
	}
	switch code := binary.LittleEndian.Uint32(r[:]); code {
	case 0:
	case spiceErrPermission:
		return nil, nil, apperr.New(apperr.SecretNeeded, "The SPICE server refused the password").With("reason", "wrong")
	default:
		return nil, nil, spiceLinkError(code)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reply, nil
}

// RelaySpice relays one SPICE channel stream. It reads the page's link
// message and has open return a server connection Go has already signed in
// to for it, with the server's link reply. The page gets that reply, its
// own ticket (sealed with an empty password) is read and dropped, and it
// is told the link succeeded; then bytes flow both ways until either side
// ends. The server connection is closed before RelaySpice returns.
func RelaySpice(page Page, open func(link []byte) (net.Conn, []byte, error)) error {
	pc := &pageConn{p: page}
	link, err := readSpiceLink(pc)
	if err != nil {
		return err
	}
	server, reply, err := open(link)
	if err != nil {
		return err
	}
	defer server.Close()
	if _, err := pc.Write(reply); err != nil {
		return err
	}
	n := spiceTicketLen
	if authSelection(link) {
		n += 4
	}
	if _, err := io.ReadFull(pc, make([]byte, n)); err != nil {
		return err
	}
	if _, err := pc.Write([]byte{0, 0, 0, 0}); err != nil {
		return err
	}
	return pipe(pc, server)
}
