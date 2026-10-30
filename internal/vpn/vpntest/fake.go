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
	// Cert connects only when the start's config pins Pin: no pin fails as
	// cert-unknown, another pin as cert-changed.
	Cert = "cert"
	// CrashThenCert: the first start connects, then exits; later starts fail
	// as cert-changed.
	CrashThenCert = "crashthencert"
	Browser       = "browser"    // waits on a browser sign-in at the URL SetURL set
	Node          = "node"       // reports NodeUp once connected and NodeClosing as it exits
	SlowExit      = "slowexit"   // takes a while to exit; a start while another runs writes an overlap mark
	SystemWide    = "systemwide" // fails as needs-system-wide
)

// What the fake reports once connected.
const (
	Code    = "246810"
	Port    = 41080
	Address = "10.9.0.2/32"
	Route   = "10.9.0.0/24"
	// BrowserPort is what an OK helper answers openBrowser with.
	BrowserPort = 41081
	// Pin is the fake gateway's certificate.
	Pin = "pin-sha256:RmFrZS1nYXRld2F5LWNlcnRpZmljYXRlLXBpbi0wMDA="
	// The node states Node reports, base64 like a real store's.
	NodeUp      = "bm9kZS1rZXlzLXVw"
	NodeClosing = "bm9kZS1rZXlzLWNsb3Npbmc="
)

const (
	name     = "hostbay-vpn" // internal/vpn.HelperName, which this package cannot import
	modeFile = "mode"
	portFile = "socksport"
	urlFile  = "url"
	runFile  = "running"
	overlap  = "overlap"
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

// SetURL is the sign-in URL Browser sends.
func (f *Fake) SetURL(t testing.TB, url string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.Dir, urlFile), []byte(url), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Overlapped reports whether a SlowExit helper started while another was
// still running.
func (f *Fake) Overlapped() bool {
	_, err := os.Stat(filepath.Join(f.Dir, overlap))
	return err == nil
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
	certFail := func(pinned string) int {
		code := proto.CodeCertUnknown
		if pinned != "" {
			code = proto.CodeCertChanged
		}
		send(proto.Msg{T: proto.State, State: proto.Failed, Code: code, ServerCert: Pin, Reason: "The gateway's certificate is not trusted."})
		return 1
	}
	switch mode {
	case Cert:
		var c struct {
			ServerCert string `json:"serverCert"`
		}
		json.Unmarshal([]byte(start.Config), &c)
		if c.ServerCert != Pin {
			return certFail(c.ServerCert)
		}
	case CrashThenCert:
		if n > 1 {
			return certFail(Pin + "x")
		}
		send(connected)
		time.Sleep(50 * time.Millisecond)
		return 1
	case Browser:
		url, _ := os.ReadFile(filepath.Join(dir, urlFile))
		send(proto.Msg{T: proto.Need, Need: proto.NeedBrowser, URL: string(url)})
		for in.Scan() {
		}
		return 0
	case Node:
		send(connected)
		send(proto.Msg{T: proto.NodeState, NodeState: NodeUp})
		for in.Scan() {
		}
		send(proto.Msg{T: proto.NodeState, NodeState: NodeClosing}) // a node writes as it closes
		return 0
	case SlowExit:
		lock, err := os.OpenFile(filepath.Join(dir, runFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			os.WriteFile(filepath.Join(dir, overlap), nil, 0o600)
		} else {
			lock.Close()
		}
		send(connected)
		for in.Scan() {
		}
		time.Sleep(300 * time.Millisecond)
		os.Remove(filepath.Join(dir, runFile))
		return 0
	case SystemWide:
		send(proto.Msg{T: proto.State, State: proto.Failed, Code: proto.CodeNeedsSystemWide, Reason: "Not here."})
		return 1
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
		var m proto.Msg
		if json.Unmarshal(in.Bytes(), &m) == nil && m.T == proto.OpenBrowser {
			send(proto.Msg{T: proto.BrowserPort, SocksPort: BrowserPort})
		}
	}
	return 0
}
