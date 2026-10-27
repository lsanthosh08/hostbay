package terminal

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"go.bug.st/serial"
)

// fakePort stands in for a serial device: the test plays the device through
// dev, reading what the terminal sent and writing what the device answers.
type fakePort struct {
	serial.Port // methods the transport must not call panic

	in  *io.PipeReader // device output, read by the transport
	out *io.PipeWriter // what the transport writes to the device

	mu     sync.Mutex
	closed bool
}

type device struct {
	say  *io.PipeWriter // device output
	hear *io.PipeReader // what the terminal sent
}

func newFakePort() (*fakePort, *device) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	return &fakePort{in: inR, out: outW}, &device{say: inW, hear: outR}
}

func (p *fakePort) Read(b []byte) (int, error)  { return p.in.Read(b) }
func (p *fakePort) Write(b []byte) (int, error) { return p.out.Write(b) }
func (p *fakePort) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.in.Close()
	p.out.Close()
	return nil
}

func (p *fakePort) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// fakeSerial replaces the enumerator and opener with one fake port named
// /dev/ttyFAKE0 and records the mode it was opened with.
func fakeSerial(t *testing.T) (*fakePort, *device, *serial.Mode) {
	t.Helper()
	port, dev := newFakePort()
	mode := &serial.Mode{}
	oldList, oldOpen := serialPorts, serialOpen
	serialPorts = func() ([]string, error) { return []string{"/dev/ttyFAKE0"}, nil }
	serialOpen = func(name string, m *serial.Mode) (serial.Port, error) {
		if name != "/dev/ttyFAKE0" {
			t.Errorf("opened %q", name)
		}
		*mode = *m
		return port, nil
	}
	t.Cleanup(func() { serialPorts, serialOpen = oldList, oldOpen })
	return port, dev, mode
}

func goodSerial() SerialConfig {
	return SerialConfig{Port: "/dev/ttyFAKE0", Baud: 9600, DataBits: 8, Parity: "none", StopBits: "1"}
}

func TestSerialRejectsBadSettingsBeforeOpening(t *testing.T) {
	cases := map[string]func(*SerialConfig){
		"baud 0":          func(c *SerialConfig) { c.Baud = 0 },
		"negative baud":   func(c *SerialConfig) { c.Baud = -9600 },
		"9 data bits":     func(c *SerialConfig) { c.DataBits = 9 },
		"4 data bits":     func(c *SerialConfig) { c.DataBits = 4 },
		"unknown parity":  func(c *SerialConfig) { c.Parity = "sometimes" },
		"3 stop bits":     func(c *SerialConfig) { c.StopBits = "3" },
		"unlisted path":   func(c *SerialConfig) { c.Port = "/etc/passwd" },
		"empty port name": func(c *SerialConfig) { c.Port = "" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			fakeSerial(t)
			opened := false
			serialOpen = func(string, *serial.Mode) (serial.Port, error) {
				opened = true
				return nil, errors.New("should not open")
			}
			c := goodSerial()
			edit(&c)
			if _, err := OpenSerial(c); err == nil {
				t.Fatal("accepted")
			}
			if opened {
				t.Fatal("opened the port before rejecting the settings")
			}
		})
	}
}

func TestSerialOpensWithChosenMode(t *testing.T) {
	_, _, mode := fakeSerial(t)
	c := goodSerial()
	c.Baud, c.DataBits, c.Parity, c.StopBits = 115200, 7, "even", "2"
	tr, err := OpenSerial(c)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	want := serial.Mode{BaudRate: 115200, DataBits: 7, Parity: serial.EvenParity, StopBits: serial.TwoStopBits}
	if *mode != want {
		t.Fatalf("mode %+v, want %+v", *mode, want)
	}
}

func TestSerialReadWriteAndClose(t *testing.T) {
	port, dev, _ := fakeSerial(t)
	tr, err := OpenSerial(goodSerial())
	if err != nil {
		t.Fatal(err)
	}
	out := readAll(tr)

	go tr.Write([]byte("AT\r"))
	got := make([]byte, 3)
	if _, err := io.ReadFull(dev.hear, got); err != nil || string(got) != "AT\r" {
		t.Fatalf("device heard %q, %v", got, err)
	}
	dev.say.Write([]byte("OK\r\n"))

	time.Sleep(50 * time.Millisecond) // let the read land before closing
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if s := <-out; s != "OK\r\n" {
		t.Fatalf("read %q", s)
	}
	if !port.isClosed() {
		t.Fatal("port left open")
	}
	if r := tr.(Ender).EndReason(); r != "session closed" {
		t.Fatalf("end reason %q", r)
	}
}

func TestSerialLocalEchoShowsTypedBytes(t *testing.T) {
	_, dev, _ := fakeSerial(t)
	c := goodSerial()
	c.LocalEcho = true
	tr, err := OpenSerial(c)
	if err != nil {
		t.Fatal(err)
	}
	out := readAll(tr)
	go io.Copy(io.Discard, dev.hear)

	if _, err := tr.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	tr.Close()
	if s := <-out; s != "hi" {
		t.Fatalf("read %q, want the typed bytes echoed", s)
	}
}

// Unplugging the adapter makes the port's read fail; the tab must close with
// a message instead of hanging.
func TestSerialUnplugClosesTerminalWithMessage(t *testing.T) {
	port, _, _ := fakeSerial(t)
	tr, err := OpenSerial(goodSerial())
	if err != nil {
		t.Fatal(err)
	}
	reasons := make(chan string, 1)
	reg := NewRegistry(func(string, string) {}, func(_, r string) { reasons <- r })
	id := reg.Open("owner", tr)
	reg.Attach(id)

	port.in.CloseWithError(errors.New("input/output error"))
	select {
	case r := <-reasons:
		if !strings.Contains(r, "disconnected") {
			t.Fatalf("reason %q", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminal did not close after the device went away")
	}
	if !port.isClosed() {
		t.Fatal("port left open")
	}
}

func TestListSerialPortsReturnsEnumeratedNames(t *testing.T) {
	fakeSerial(t)
	ports, err := ListSerialPorts()
	if err != nil || len(ports) != 1 || ports[0] != "/dev/ttyFAKE0" {
		t.Fatalf("%v %v", ports, err)
	}
}

// The tap reads what the device sent, before local echo joins it, so a
// typed password reaches the screen but never a log or the agent (KTD38).
func TestSerialLocalEchoNeverReachesTheTap(t *testing.T) {
	_, dev, _ := fakeSerial(t)
	c := goodSerial()
	c.LocalEcho = true
	tr, err := OpenSerial(c)
	if err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, dev.hear)
	r, l := newLoggedRegistry()
	id := r.Open("owner", tr)
	r.Attach(id)
	dev.say.Write([]byte("Password: "))
	waitFor(t, "the prompt in the tap", func() bool { b, _ := r.Recent(id); return string(b) == "Password: " })
	if err := r.Write(id, []byte("hunter2")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the echo on screen", func() bool { return strings.Contains(strings.Join(l.get(), ""), "hunter2") })
	if b, _ := r.Recent(id); strings.Contains(string(b), "hunter2") {
		t.Fatalf("the tap holds the typed password: %q", b)
	}
	r.Close(id)
}
