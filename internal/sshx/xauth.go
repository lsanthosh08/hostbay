package sshx

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
)

// xauth entry families (Xauth.h).
const (
	familyInternet  = 0
	familyInternet6 = 6
	familyLocal     = 256
	familyWild      = 65535
)

// xauthFile is the local X authority file: $XAUTHORITY, else
// ~/.Xauthority.
func xauthFile() string {
	if p := os.Getenv("XAUTHORITY"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".Xauthority")
}

// readCookie returns the MIT-MAGIC-COOKIE-1 in the authority file at path
// that applies to d, the first that matches as libXau picks it. A missing
// file or no matching entry is a nil cookie: the X server may not check
// one, as VcXsrv started with -ac does not.
func readCookie(path string, d xDisplay) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		var fam [2]byte
		if _, err := io.ReadFull(r, fam[:]); err == io.EOF {
			return nil, nil
		} else if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		var fields [4][]byte // address, number, name, data
		for i := range fields {
			if fields[i], err = readField(r); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		}
		if string(fields[2]) == x11Proto && d.matches(binary.BigEndian.Uint16(fam[:]), fields[0], string(fields[1])) {
			return fields[3], nil
		}
	}
}

func readField(r io.Reader) ([]byte, error) {
	var n [2]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, io.ErrUnexpectedEOF
	}
	b := make([]byte, binary.BigEndian.Uint16(n[:]))
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, io.ErrUnexpectedEOF
	}
	return b, nil
}

// matches reports whether an entry for family, address and display number
// (empty for any) applies to d, as libxcb decides: a socket or loopback
// display uses the local entries, a remote one the entry for its address.
func (d xDisplay) matches(family uint16, addr []byte, number string) bool {
	if number != "" && number != d.number {
		return false
	}
	switch family {
	case familyWild:
		return true
	case familyLocal:
		return d.local
	case familyInternet, familyInternet6:
		ip := net.ParseIP(d.host)
		return ip != nil && !d.local && ip.Equal(net.IP(addr))
	}
	return false
}

// wildXauth is an authority file holding one MIT-MAGIC-COOKIE-1 entry for
// any address and display, so it fits whichever display Xvnc ends up on.
func wildXauth(cookie []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, familyWild)
	for _, f := range [][]byte{nil, nil, []byte(x11Proto), cookie} {
		b = binary.BigEndian.AppendUint16(b, uint16(len(f)))
		b = append(b, f...)
	}
	return b
}
