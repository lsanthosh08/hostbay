package rdp

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"hostbay/internal/apperr"
)

var fakeHelper string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fakehelper")
	if err != nil {
		panic(err)
	}
	fakeHelper = filepath.Join(dir, "fakehelper")
	if runtime.GOOS == "windows" {
		fakeHelper += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", fakeHelper, "./testdata/fakehelper").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const testPassword = "Rdp-Secret-42!"

func testConnect() Connect {
	return Connect{DialHost: "127.0.0.1", DialPort: 50000, TargetHost: "win.lan", User: "me", Password: testPassword, Width: 4, Height: 2, Security: "auto"}
}

type recorder struct {
	certs   chan []byte
	texts   chan string
	exited  chan string
	certErr error
}

func newRecorder() *recorder {
	return &recorder{certs: make(chan []byte, 4), texts: make(chan string, 16), exited: make(chan string, 1)}
}

func (r *recorder) hooks() Hooks {
	return Hooks{
		CheckCert:     func(der []byte) error { r.certs <- der; return r.certErr },
		ClipboardText: func(t string) { r.texts <- t },
		Exited:        func(reason string) { r.exited <- reason },
	}
}

func start(t *testing.T, mode string, r *recorder) (*Session, error) {
	t.Helper()
	t.Setenv("FAKE_RDP_MODE", mode)
	s, err := Start(context.Background(), fakeHelper, testConnect(), r.hooks())
	if s != nil {
		t.Cleanup(s.Close)
	}
	return s, err
}

func recv[T any](t *testing.T, c chan T) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
		panic("unreachable")
	}
}

// collect attaches and returns every message the UI would get.
func collect(t *testing.T, s *Session) [][]byte {
	t.Helper()
	var got [][]byte
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.Attach(ctx, func(b []byte) error {
		got = append(got, b)
		if b[0] == MsgClosed {
			cancel()
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	return got
}

func TestHandshakeTrustAndHeldFramesReachTheUI(t *testing.T) {
	r := newRecorder()
	s, err := start(t, "", r)
	if err != nil {
		t.Fatal(err)
	}
	if der := recv(t, r.certs); string(der) != "fake-cert" {
		t.Fatalf("cert = %q", der)
	}

	// The password crossed stdin only: the helper saw it in connect, and
	// not in its arguments or environment.
	var seen struct{ Args, Env, Password string }
	if err := json.Unmarshal([]byte(recv(t, r.texts)), &seen); err != nil {
		t.Fatal(err)
	}
	if seen.Password != testPassword {
		t.Fatalf("helper got password %q", seen.Password)
	}
	if strings.Contains(seen.Args, testPassword) || strings.Contains(seen.Env, testPassword) {
		t.Fatal("the password reached the helper's arguments or environment")
	}

	// Frames sent before anyone subscribed were held, in order.
	if err := s.Input("move", 3, 1, 0, false, 0, 0); err != nil {
		t.Fatal(err)
	}
	if echo := recv(t, r.texts); !strings.Contains(echo, `"t":"input"`) || !strings.Contains(echo, `"x":3`) {
		t.Fatalf("input forwarded as %s", echo)
	}
	s.Close()
	// After a requested close the closed message may be dropped: nobody
	// is owed news of their own close.
	got := collect(t, s)
	if len(got) == 4 && got[3][0] == MsgClosed {
		got = got[:3]
	}
	if len(got) != 3 || got[0][0] != MsgSize || got[1][0] != MsgFrame || got[2][0] != MsgFrame {
		t.Fatalf("UI got %d messages: %v", len(got), kinds(got))
	}
	if w, h := binary.LittleEndian.Uint16(got[0][1:]), binary.LittleEndian.Uint16(got[0][3:]); w != 4 || h != 2 {
		t.Fatalf("size %dx%d", w, h)
	}
	f := got[2]
	if x, y, w, h := binary.LittleEndian.Uint16(f[1:]), binary.LittleEndian.Uint16(f[3:]), binary.LittleEndian.Uint16(f[5:]), binary.LittleEndian.Uint16(f[7:]); x != 1 || y != 1 || w != 1 || h != 1 || len(f) != 9+4 || f[9] != 0xBB {
		t.Fatalf("frame %v", f)
	}
	if reason := recv(t, r.exited); reason != "" {
		t.Fatalf("a requested close reported %q", reason)
	}
}

func kinds(msgs [][]byte) []byte {
	var k []byte
	for _, m := range msgs {
		k = append(k, m[0])
	}
	return k
}

func TestVersionMismatchIsRefused(t *testing.T) {
	_, err := start(t, "badversion", newRecorder())
	if apperr.CodeOf(err) != apperr.HelperMissing || !strings.Contains(err.Error(), "version 99") {
		t.Fatalf("err = %v", err)
	}
}

func TestSilentHelperTimesOut(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 200 * time.Millisecond
	defer func() { handshakeTimeout = old }()
	_, err := start(t, "silent", newRecorder())
	if apperr.CodeOf(err) != apperr.HelperMissing {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingHelperIsHelperMissing(t *testing.T) {
	_, err := Start(context.Background(), filepath.Join(t.TempDir(), "nope"), testConnect(), newRecorder().hooks())
	if apperr.CodeOf(err) != apperr.HelperMissing {
		t.Fatalf("err = %v", err)
	}
	if _, err := Start(context.Background(), "fakehelper", testConnect(), newRecorder().hooks()); apperr.CodeOf(err) != apperr.HelperMissing {
		t.Fatalf("a relative path was accepted: %v", err)
	}
}

func TestRefusedCertificateEndsTheSession(t *testing.T) {
	r := newRecorder()
	r.certErr = apperr.New(apperr.HostKeyUnknown, "unknown certificate")
	_, err := start(t, "", r)
	if apperr.CodeOf(err) != apperr.HostKeyUnknown {
		t.Fatalf("err = %v", err)
	}
}

func TestHelperErrorDuringConnectIsReported(t *testing.T) {
	_, err := start(t, "fail", newRecorder())
	if err == nil || !strings.Contains(err.Error(), "logon failure") {
		t.Fatalf("err = %v", err)
	}
}

func TestCrashIsReportedAndReaped(t *testing.T) {
	r := newRecorder()
	s, err := start(t, "crash", r)
	if err != nil {
		t.Fatal(err)
	}
	if reason := recv(t, r.exited); !strings.Contains(reason, "exit status 7") {
		t.Fatalf("reason = %q", reason)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the helper was not reaped")
	}
	got := collect(t, s)
	last := got[len(got)-1]
	if last[0] != MsgClosed || !strings.Contains(string(last[1:]), "exit status 7") {
		t.Fatalf("UI did not hear about the crash: %q", last)
	}
	if err := s.Input("move", 0, 0, 0, false, 0, 0); err == nil {
		t.Fatal("input to a dead session succeeded")
	}
}

func TestMalformedFrameStopsTheHelper(t *testing.T) {
	r := newRecorder()
	if _, err := start(t, "badframe", r); err != nil {
		t.Fatal(err)
	}
	if reason := recv(t, r.exited); !strings.Contains(reason, "malformed") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestInputIsValidatedBeforeForwarding(t *testing.T) {
	r := newRecorder()
	s, err := start(t, "", r)
	if err != nil {
		t.Fatal(err)
	}
	recv(t, r.texts) // the report
	bad := []struct {
		kind                          string
		x, y, button, scancode, delta int
	}{
		{"teleport", 0, 0, 0, 0, 0},
		{"move", 4, 0, 0, 0, 0}, // desktop is 4x2
		{"move", 0, -1, 0, 0, 0},
		{"button", 0, 0, 3, 0, 0},
		{"key", 0, 0, 0, 0x1234, 0},
		{"key", 0, 0, 0, 0, 0},
		{"wheel", 0, 0, 0, 0, 40000},
	}
	for _, b := range bad {
		if err := s.Input(b.kind, b.x, b.y, b.button, false, b.scancode, b.delta); err == nil {
			t.Errorf("%+v was accepted", b)
		}
	}
	if err := s.SetClipboardText("bad \xff utf-8"); err == nil {
		t.Error("invalid UTF-8 was accepted")
	}
	for _, paths := range [][]string{nil, {"relative/file"}, {"/no/such/file/here"}, {"/tmp/../etc/passwd"}} {
		if err := s.SetClipboardFiles(paths); err == nil {
			t.Errorf("%q was accepted", paths)
		}
	}
	file := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(file, []byte("x"), 0o600)
	if err := s.SetClipboardFiles([]string{file}); err != nil {
		t.Fatal(err)
	}
	if err := s.Input("key", 0, 0, 0, true, 0xe04d, 0); err != nil {
		t.Fatal(err)
	}
	// Only the two valid messages reached the helper.
	if echo := recv(t, r.texts); !strings.Contains(echo, `"t":"clipboardFiles"`) {
		t.Fatalf("got %s", echo)
	}
	if echo := recv(t, r.texts); !strings.Contains(echo, `"scancode":57421`) {
		t.Fatalf("got %s", echo)
	}
}

func TestSecondAttachIsRefused(t *testing.T) {
	s, err := start(t, "", newRecorder())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.Attach(ctx, func([]byte) error { return nil })
	defer cancel()
	time.Sleep(50 * time.Millisecond)
	if err := s.Attach(context.Background(), func([]byte) error { return nil }); err == nil {
		t.Fatal("a second subscriber attached")
	}
}

func TestHeldFramesStayWithinByteBudget(t *testing.T) {
	s, err := start(t, "big", newRecorder())
	if err != nil {
		t.Fatal(err)
	}
	const frameLen = 9 + 1920*1080*4
	var most int64
	var full time.Time
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		most = max(most, s.queued.Load())
		if most > heldBytes {
			t.Fatalf("held %d bytes, cap %d", most, heldBytes)
		}
		if full.IsZero() && most > heldBytes-frameLen {
			full = time.Now()
		}
		if !full.IsZero() && time.Since(full) > 200*time.Millisecond {
			break
		}
	}
	if full.IsZero() {
		t.Fatalf("queue never filled: most %d bytes", most)
	}
	frames := 0
	for _, b := range collect(t, s) {
		if b[0] == MsgFrame {
			frames++
		}
	}
	if frames != 20 {
		t.Fatalf("got %d frames, want 20", frames)
	}
}
