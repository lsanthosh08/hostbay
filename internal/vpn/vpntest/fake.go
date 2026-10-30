// Package vpntest is a stand-in for hostbay-vpn in tests. It re-executes
// the test binary under the name hostbay-vpn, so no test brings up a real
// tunnel. The fake speaks internal/vpn/proto, behaves as its mode file
// says, and records every start message it was sent.
//
// A package that uses it calls MaybeRun first thing in its TestMain.
package vpntest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"hostbay/internal/vpn/proto"
)

// Modes.
const (
	OK         = "ok"         // connects and stays up until stdin closes
	OTP        = "otp"        // asks for a one-time code; connects only on Code
	Crash      = "crash"      // the first start connects, then exits 1 without a word; later starts are OK
	Silent     = "silent"     // never says hello
	BadVersion = "badversion" // says hello with another protocol version
	Down       = "down"       // never connects: the gateway does not answer
)

// What the fake reports once connected.
const (
	Code    = "246810"
	Port    = 41080
	Address = "10.9.0.2/32"
	Route   = "10.9.0.0/24"
)

const (
	name     = "hostbay-vpn" // internal/vpn.HelperName, which this package cannot import
	modeFile = "mode"
	portFile = "socksport"
	startDir = "starts"
)

// Fake is an installed fake helper.
type Fake struct {
	Path string // the executable named hostbay-vpn
	Dir  string // its folder, which also holds the mode and the start log
}

// MaybeRun runs the fake and exits when this binary was started under the
// helper's name; otherwise it returns.
func MaybeRun() {
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == name {
		os.Exit(run())
	}
}

// Install puts a fake helper in mode in a new folder.
func Install(t testing.TB, mode string) *Fake {
	t.Helper()
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := &Fake{Path: filepath.Join(dir, name), Dir: dir}
	if runtime.GOOS == "windows" {
		f.Path += ".exe"
	}
	// A copy, not a link: a symlink would resolve to the test binary's name.
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, startDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, modeFile), []byte(mode), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// SetSocksPort makes the fake report port, where a test serves SOCKS5, in
// place of Port.
func (f *Fake) SetSocksPort(t testing.TB, port int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.Dir, portFile), []byte(strconv.Itoa(port)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Starts returns every start message the fake was sent, in order.
func (f *Fake) Starts(t testing.TB) []proto.Msg {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(f.Dir, startDir, "*.json"))
	sort.Strings(files)
	out := []proto.Msg{}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var m proto.Msg
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func run() int {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	b, _ := os.ReadFile(filepath.Join(dir, modeFile))
	mode := string(b)
	out := json.NewEncoder(os.Stdout)
	send := func(m proto.Msg) { out.Encode(m) }

	switch mode {
	case Silent:
		time.Sleep(time.Minute)
		return 0
	case BadVersion:
		send(proto.Msg{T: proto.Hello, Version: proto.Version + 98})
		time.Sleep(time.Minute)
		return 0
	}
	send(proto.Msg{T: proto.Hello, Version: proto.Version})
	in := bufio.NewScanner(os.Stdin)
	var start proto.Msg
	if !in.Scan() || json.Unmarshal(in.Bytes(), &start) != nil || start.T != proto.Start {
		return 2
	}
	starts, _ := filepath.Glob(filepath.Join(dir, startDir, "*.json"))
	n := len(starts) + 1
	os.WriteFile(filepath.Join(dir, startDir, fmt.Sprintf("%04d.json", n)), in.Bytes(), 0o600)

	port := Port
	if b, err := os.ReadFile(filepath.Join(dir, portFile)); err == nil {
		port, _ = strconv.Atoi(string(b))
	}
	connected := proto.Msg{T: proto.State, State: proto.Connected, Address: []string{Address}, Routes: []string{Route}, SocksPort: port}
	send(proto.Msg{T: proto.State, State: proto.Connecting})
	switch mode {
	case Down:
		send(proto.Msg{T: proto.State, State: proto.Failed, Reason: "The gateway did not answer."})
		return 1
	case OTP:
		send(proto.Msg{T: proto.Need, Need: proto.NeedOTP})
		var a proto.Msg
		if !in.Scan() || json.Unmarshal(in.Bytes(), &a) != nil || a.T != proto.Answer || a.OTP != Code {
			send(proto.Msg{T: proto.State, State: proto.Failed, Reason: "The one-time code was refused."})
			return 1
		}
		send(proto.Msg{T: proto.State, State: proto.Connecting})
	case Crash:
		if n == 1 {
			send(connected)
			time.Sleep(50 * time.Millisecond)
			return 1
		}
	}
	send(connected)
	for in.Scan() {
	}
	return 0
}
