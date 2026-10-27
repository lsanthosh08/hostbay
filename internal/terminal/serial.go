package terminal

// Serial terminals (R20, KTD10) learn from uniterm's
// backend/session/serial_session.go (Apache-2.0, github.com/ys-ll/uniterm).
// Unlike that one, only a port the OS enumerates can be opened, every
// setting is checked before the port is touched, and local echo joins the
// same output stream as the device so the registry paces both.

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"

	"go.bug.st/serial"
)

// SerialConfig is what the user picks for a serial terminal.
type SerialConfig struct {
	Port      string // as ListSerialPorts names it
	Baud      int
	DataBits  int    // 5 to 8
	Parity    string // none, odd, even, mark or space
	StopBits  string // 1, 1.5 or 2
	LocalEcho bool   // show typed bytes, for devices that do not echo
}

var parities = map[string]serial.Parity{
	"none": serial.NoParity, "odd": serial.OddParity, "even": serial.EvenParity,
	"mark": serial.MarkParity, "space": serial.SpaceParity,
}

var stopBits = map[string]serial.StopBits{
	"1": serial.OneStopBit, "1.5": serial.OnePointFiveStopBits, "2": serial.TwoStopBits,
}

// The OS's serial ports; tests swap in a fake device.
var (
	serialPorts = serial.GetPortsList
	serialOpen  = serial.Open
)

// ListSerialPorts names the serial ports this computer has now.
func ListSerialPorts() ([]string, error) {
	ports, err := serialPorts()
	if err != nil {
		return nil, fmt.Errorf("listing serial ports: %w", err)
	}
	return ports, nil
}

// mode checks every setting, so nothing invalid reaches the driver.
func (c SerialConfig) mode() (*serial.Mode, error) {
	if c.Baud <= 0 {
		return nil, fmt.Errorf("baud rate %d is not valid", c.Baud)
	}
	if c.DataBits < 5 || c.DataBits > 8 {
		return nil, fmt.Errorf("data bits must be 5, 6, 7 or 8, not %d", c.DataBits)
	}
	p, ok := parities[c.Parity]
	if !ok {
		return nil, fmt.Errorf("parity %q is not none, odd, even, mark or space", c.Parity)
	}
	s, ok := stopBits[c.StopBits]
	if !ok {
		return nil, fmt.Errorf("stop bits must be 1, 1.5 or 2, not %q", c.StopBits)
	}
	return &serial.Mode{BaudRate: c.Baud, DataBits: c.DataBits, Parity: p, StopBits: s}, nil
}

// OpenSerial opens one of the listed ports with c's settings. A name that
// is not listed is refused, so the UI cannot open an arbitrary path.
func OpenSerial(c SerialConfig) (Transport, error) {
	m, err := c.mode()
	if err != nil {
		return nil, err
	}
	ports, err := ListSerialPorts()
	if err != nil {
		return nil, err
	}
	if !slices.Contains(ports, c.Port) {
		return nil, fmt.Errorf("%q is not a serial port on this computer", c.Port)
	}
	p, err := serialOpen(c.Port, m)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", c.Port, err)
	}
	return newStreamTerm(p, c.LocalEcho, "the serial device was disconnected"), nil
}

// streamTerm is a terminal on a plain byte stream, a serial port or a raw
// TCP connection, with optional local echo.
type streamTerm struct {
	port io.ReadWriteCloser
	echo bool
	lost string         // the end reason when the far side goes away
	r    *io.PipeReader // device output and local echo, in order
	w    *io.PipeWriter
	tap  atomic.Pointer[tap] // sees device output only; set by Registry.Open

	mu     sync.Mutex
	closed bool
	end    string
}

func newStreamTerm(port io.ReadWriteCloser, echo bool, lost string) *streamTerm {
	pr, pw := io.Pipe()
	s := &streamTerm{port: port, echo: echo, lost: lost, r: pr, w: pw}
	go s.pump()
	return s
}

// pump copies the device's output until the port fails or is closed,
// showing it to the tap before local echo joins it (KTD38).
func (s *streamTerm) pump() {
	_, err := io.Copy(s.w, io.TeeReader(s.port, deviceTap{s}))
	s.mu.Lock()
	if !s.closed && s.end == "" {
		s.end = s.lost
		if err != nil && !errors.Is(err, io.EOF) {
			s.end += " (" + err.Error() + ")"
		}
	}
	s.mu.Unlock()
	s.port.Close()
	s.w.Close()
}

// deviceTap hands device output to the terminal's tap, once there is one.
type deviceTap struct{ s *streamTerm }

func (d deviceTap) Write(p []byte) (int, error) {
	if t := d.s.tap.Load(); t != nil {
		t.Write(p)
	}
	return len(p), nil
}

func (s *streamTerm) Read(b []byte) (int, error) { return s.r.Read(b) }

func (s *streamTerm) Write(b []byte) (int, error) {
	n, err := s.port.Write(b)
	if err == nil && s.echo {
		s.w.Write(b[:n])
	}
	return n, err
}

// Resize does nothing: a serial line has no window size.
func (*streamTerm) Resize(int, int) error { return nil }

func (s *streamTerm) Close() error {
	s.mu.Lock()
	first := !s.closed
	s.closed = true
	s.mu.Unlock()
	if !first {
		return nil
	}
	err := s.port.Close() // ends pump, which closes the output
	s.w.Close()
	return err
}

func (s *streamTerm) EndReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.end == "" {
		return "session closed"
	}
	return s.end
}
