package remotedesktop

import (
	"crypto/aes"
	"crypto/des"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/bits"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"hostbay/internal/apperr"
)

// handshakeTimeout bounds the RFB sign-in, so a silent server cannot hang
// a connect.
var handshakeTimeout = 20 * time.Second

// bound closes conn once handshakeTimeout passes, for a conn that ignores
// deadlines, as an SSH channel through a jump host does. The returned stop
// turns a sign-in the timer cut short into an error.
func bound(conn net.Conn, err *error) (stop func()) {
	t := time.AfterFunc(handshakeTimeout, func() { conn.Close() })
	return func() {
		if !t.Stop() && *err == nil {
			*err = errors.New("the sign-in timed out")
		}
	}
}

// RFB security types and VeNCrypt subtypes (RFC 6143 and the RFB
// community registry).
const (
	secNone      = 1
	secVNC       = 2
	secVeNCrypt  = 19
	secARD       = 30
	subX509None  = 260
	subX509VNC   = 261
	subX509Plain = 262
)

var secNames = map[uint32]string{
	1: "None", 2: "VNC password", 5: "RA2", 6: "RA2ne", 16: "Tight", 17: "Ultra", 18: "TLS",
	19: "VeNCrypt", 20: "SASL", 22: "XVP", 30: "Apple Remote Desktop", 113: "MSLogonII", 129: "Unix Logon",
	256: "Plain", 257: "TLSNone", 258: "TLSVnc", 259: "TLSPlain",
	260: "X509None", 261: "X509Vnc", 262: "X509Plain",
}

func names(types []uint32) string {
	s := make([]string, len(types))
	for i, t := range types {
		if n, ok := secNames[t]; ok {
			s[i] = n
		} else {
			s[i] = "type " + strconv.Itoa(int(t))
		}
	}
	return strings.Join(s, ", ")
}

// VNCAuth is how Go signs in to a VNC server.
type VNCAuth struct {
	Security string      // "vencrypt" or "none" (store.Host.VNCSecurity)
	User     string      // when set, Apple Remote Desktop, or VeNCrypt's X509Plain
	Password string      // never sent to the page
	TLS      *tls.Config // VeNCrypt's; from tlstrust, never unverified
}

// SignIn runs the RFB handshake on conn through the security result, in
// a's security mode (KTD18): VeNCrypt with an X509 subtype only, or, for
// "none", Apple Remote Desktop (with a user name), VNC password or None.
// A server that offers none of those is refused, naming what it offered,
// before anything is answered. It returns the connection to relay, wrapped
// in TLS under VeNCrypt. A refused sign-in is SecretNeeded. The caller
// closes conn on error.
func SignIn(conn net.Conn, a VNCAuth) (_ net.Conn, err error) {
	var want []uint32
	switch {
	case a.Security == "vencrypt":
		want = []uint32{secVeNCrypt}
	case a.Security != "none":
		return nil, fmt.Errorf("unknown VNC security mode %q", a.Security)
	case a.User != "":
		want = []uint32{secARD, secVNC, secNone}
	default:
		want = []uint32{secVNC, secNone}
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	defer bound(conn, &err)()

	var v [12]byte
	if _, err := io.ReadFull(conn, v[:]); err != nil {
		return nil, fmt.Errorf("the VNC server did not greet: %w", err)
	}
	minor, err := strconv.Atoi(string(v[8:11]))
	if string(v[:8]) != "RFB 003." || v[11] != '\n' || err != nil {
		return nil, fmt.Errorf("not a VNC server (it sent %q)", v[:])
	}
	// 3.889 is macOS Screen Sharing's 3.8; unknown minors mean 3.3.
	switch {
	case minor >= 8:
		minor = 8
	case minor != 7:
		minor = 3
	}
	if _, err := fmt.Fprintf(conn, "RFB 003.%03d\n", minor); err != nil {
		return nil, err
	}

	var offered []uint32
	if minor == 3 { // the server chooses
		t, err := readU32(conn)
		if err == nil && t == 0 {
			err = refused(conn, "the VNC server refused the connection")
		}
		if err != nil {
			return nil, err
		}
		offered = []uint32{t}
	} else {
		var n [1]byte
		if _, err := io.ReadFull(conn, n[:]); err != nil {
			return nil, err
		}
		if n[0] == 0 {
			return nil, refused(conn, "the VNC server refused the connection")
		}
		b := make([]byte, n[0])
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, err
		}
		for _, t := range b {
			offered = append(offered, uint32(t))
		}
	}
	t := pick(offered, want)
	if t == 0 {
		msg := fmt.Sprintf("the VNC server offers %s, but this host signs in with %s", names(offered), names(want))
		if slices.Contains(offered, secARD) && a.Security == "none" && a.User == "" {
			msg += "; set a user name on the host for Apple Remote Desktop"
		}
		return nil, errors.New(msg)
	}
	if minor >= 7 {
		if _, err := conn.Write([]byte{byte(t)}); err != nil {
			return nil, err
		}
	}

	switch t {
	case secVNC:
		err = vncAuth(conn, a.Password)
	case secARD:
		err = ardAuth(conn, a.User, a.Password)
	case secVeNCrypt:
		conn, err = veNCrypt(conn, a)
	}
	// RFB 3.3 and 3.7 send no result after None.
	if err == nil && (t != secNone || minor == 8) {
		var r uint32
		if r, err = readU32(conn); err == nil && r != 0 {
			reason := ""
			if minor == 8 {
				reason = ": " + readReason(conn)
			}
			err = apperr.New(apperr.SecretNeeded, "The VNC server refused the sign-in"+reason).With("reason", "wrong")
		}
	}
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// pick is the first of want that the server offered, or 0.
func pick(offered, want []uint32) uint32 {
	for _, w := range want {
		if slices.Contains(offered, w) {
			return w
		}
	}
	return 0
}

func readU32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

// readReason reads a failure reason, or returns "" if it is unreadable or
// unreasonably long.
func readReason(r io.Reader) string {
	n, err := readU32(r)
	if err != nil || n > 4096 {
		return ""
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return ""
	}
	return string(b)
}

func refused(r io.Reader, msg string) error {
	if reason := readReason(r); reason != "" {
		msg += ": " + reason
	}
	return errors.New(msg)
}

// vncAuth answers the DES challenge. The key is the password's first 8
// bytes, each bit-reversed: that is the protocol.
func vncAuth(rw io.ReadWriter, password string) error {
	var ch [16]byte
	if _, err := io.ReadFull(rw, ch[:]); err != nil {
		return err
	}
	key := make([]byte, 8)
	copy(key, password)
	for i := range key {
		key[i] = bits.Reverse8(key[i])
	}
	blk, err := des.NewCipher(key)
	if err != nil {
		return err
	}
	blk.Encrypt(ch[:8], ch[:8])
	blk.Encrypt(ch[8:], ch[8:])
	_, err = rw.Write(ch[:])
	return err
}

// ardAuth is Apple Remote Desktop authentication (type 30): Diffie-Hellman
// with the server's group, then the user name and password, each
// NUL-terminated in 64 random bytes, AES-128-ECB encrypted under the MD5 of
// the shared secret.
func ardAuth(rw io.ReadWriter, user, password string) error {
	if len(user) > 63 || len(password) > 63 {
		return errors.New("Apple Remote Desktop allows user names and passwords of at most 63 bytes")
	}
	var hdr [4]byte
	if _, err := io.ReadFull(rw, hdr[:]); err != nil {
		return err
	}
	g, n := binary.BigEndian.Uint16(hdr[:2]), int(binary.BigEndian.Uint16(hdr[2:]))
	if n == 0 || n > 1024 {
		return fmt.Errorf("the server sent an Apple Remote Desktop key of %d bytes", n)
	}
	b := make([]byte, 2*n)
	if _, err := io.ReadFull(rw, b); err != nil {
		return err
	}
	p, serverPub := new(big.Int).SetBytes(b[:n]), new(big.Int).SetBytes(b[n:])
	if p.Cmp(big.NewInt(3)) < 0 {
		return errors.New("the server sent an unusable Apple Remote Desktop key")
	}
	priv, err := rand.Int(rand.Reader, p)
	if err != nil {
		return err
	}
	pub := new(big.Int).Exp(big.NewInt(int64(g)), priv, p)
	key := md5.Sum(new(big.Int).Exp(serverPub, priv, p).FillBytes(make([]byte, n)))

	var creds [128]byte
	if _, err := rand.Read(creds[:]); err != nil {
		return err
	}
	copy(creds[:], user)
	creds[len(user)] = 0
	copy(creds[64:], password)
	creds[64+len(password)] = 0
	blk, err := aes.NewCipher(key[:])
	if err != nil {
		return err
	}
	for i := 0; i < len(creds); i += 16 {
		blk.Encrypt(creds[i:i+16], creds[i:i+16])
	}
	_, err = rw.Write(append(creds[:], pub.FillBytes(make([]byte, n))...))
	return err
}

// veNCrypt negotiates VeNCrypt 0.2 with an X509 subtype, so the server's
// certificate is checked by a.TLS (the anonymous TLS subtypes cannot be),
// then signs in inside TLS.
func veNCrypt(conn net.Conn, a VNCAuth) (net.Conn, error) {
	if a.TLS == nil {
		return nil, errors.New("VeNCrypt needs a TLS config")
	}
	var v [2]byte
	if _, err := io.ReadFull(conn, v[:]); err != nil {
		return nil, err
	}
	if v != [2]byte{0, 2} {
		return nil, fmt.Errorf("the server speaks VeNCrypt %d.%d; Hostbay speaks 0.2", v[0], v[1])
	}
	if _, err := conn.Write(v[:]); err != nil {
		return nil, err
	}
	var b [2]byte // version ack, then the subtype count
	if _, err := io.ReadFull(conn, b[:]); err != nil {
		return nil, err
	}
	if b[0] != 0 {
		return nil, errors.New("the server refused VeNCrypt 0.2")
	}
	subs := make([]uint32, b[1])
	for i := range subs {
		var err error
		if subs[i], err = readU32(conn); err != nil {
			return nil, err
		}
	}
	want := []uint32{subX509VNC, subX509None}
	if a.User != "" {
		want = []uint32{subX509Plain, subX509VNC, subX509None}
	}
	sub := pick(subs, want)
	if sub == 0 {
		return nil, fmt.Errorf("the VNC server offers VeNCrypt %s, but Hostbay needs a subtype with a certificate it can check (%s)", names(subs), names(want))
	}
	if _, err := conn.Write(binary.BigEndian.AppendUint32(nil, sub)); err != nil {
		return nil, err
	}
	var ok [1]byte
	if _, err := io.ReadFull(conn, ok[:]); err != nil {
		return nil, err
	}
	if ok[0] == 0 {
		return nil, fmt.Errorf("the server refused VeNCrypt %s", names([]uint32{sub}))
	}
	tc := tls.Client(conn, a.TLS)
	if err := tc.Handshake(); err != nil {
		return nil, err // a *tls.CertificateVerificationError reaches tlstrust as is
	}
	switch sub {
	case subX509VNC:
		return tc, vncAuth(tc, a.Password)
	case subX509Plain:
		msg := binary.BigEndian.AppendUint32(nil, uint32(len(a.User)))
		msg = binary.BigEndian.AppendUint32(msg, uint32(len(a.Password)))
		_, err := tc.Write(append(append(msg, a.User...), a.Password...))
		return tc, err
	}
	return tc, nil
}

// OfferNone is the page's side of the handshake after Go has signed in:
// an RFB 3.8 server asking for no authentication. What follows is the
// signed-in server's ClientInit onward, the same in every RFB version.
func OfferNone(page io.ReadWriter) error {
	if _, err := io.WriteString(page, "RFB 003.008\n"); err != nil {
		return err
	}
	var v [12]byte
	if _, err := io.ReadFull(page, v[:]); err != nil {
		return err
	}
	if string(v[:]) != "RFB 003.008\n" {
		return fmt.Errorf("the page answered RFB version %q", v[:])
	}
	if _, err := page.Write([]byte{1, secNone}); err != nil {
		return err
	}
	var t [1]byte
	if _, err := io.ReadFull(page, t[:]); err != nil {
		return err
	}
	if t[0] != secNone {
		return fmt.Errorf("the page chose security type %d", t[0])
	}
	_, err := page.Write([]byte{0, 0, 0, 0})
	return err
}
