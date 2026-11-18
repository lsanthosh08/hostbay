package portowner

import (
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPortOwnerLsofName(t *testing.T) {
	cases := []struct {
		name, out, want string
	}{
		{"one process", "p4242\ncAWS VPN Client\nf12\n", "AWS VPN Client"},
		{"several processes take the first", "p10\ncnginx\nf6\nf7\np11\ncnginx-worker\nf6\n", "nginx"},
		{"windows line ends", "p1\r\ncacvc\r\n", "acvc"},
		{"empty output", "", ""},
		{"no command field", "p4242\nf12\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := lsofName(c.out); got != c.want {
				t.Fatalf("lsofName(%q) = %q, want %q", c.out, got, c.want)
			}
		})
	}
}

func TestPortOwnerTablePID(t *testing.T) {
	row := func(port uint16, pid uint32) []byte {
		b := make([]byte, 24)
		binary.LittleEndian.PutUint32(b[0:], 2) // LISTEN
		binary.BigEndian.PutUint16(b[8:], port) // network byte order
		binary.LittleEndian.PutUint32(b[20:], pid)
		return b
	}
	table := func(rows ...[]byte) []byte {
		b := binary.LittleEndian.AppendUint32(nil, uint32(len(rows)))
		for _, r := range rows {
			b = append(b, r...)
		}
		return b
	}
	cases := []struct {
		name  string
		table []byte
		want  uint32
	}{
		{"found", table(row(445, 4), row(35001, 7120)), 7120},
		{"absent", table(row(445, 4)), 0},
		{"empty", table(), 0},
		{"short row count", table(row(35001, 9))[:20], 0},
		{"nil", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tablePID(c.table, 35001); got != c.want {
				t.Fatalf("tablePID = %d, want %d", got, c.want)
			}
		})
	}
}

func TestPortOwnerNamesThisProcess(t *testing.T) {
	if runtime.GOOS != "windows" {
		if _, err := exec.LookPath("lsof"); err != nil {
			t.Skip("no lsof")
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	name, err := Name(ln.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Base(os.Args[0])
	if want := strings.TrimSuffix(exe, ".exe"); name != want {
		t.Fatalf("Name = %q, want %q", name, want)
	}
}

func TestPortOwnerInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = net.Listen("tcp", ln.Addr().String())
	if err == nil {
		t.Fatal("a second listen on the same port succeeded")
	}
	if !InUse(err) {
		t.Fatalf("InUse(%v) = false", err)
	}
	if InUse(os.ErrNotExist) {
		t.Fatal("InUse(ErrNotExist) = true")
	}
}
