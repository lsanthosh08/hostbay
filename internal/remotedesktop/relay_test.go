package remotedesktop

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"hostbay/internal/remotedesktop/rfbtest"
)

func asCertErr(err error, target any) bool { return err != nil && errors.As(err, target) }

// The page meets a server that asks for no authentication, its RFB then
// reaches the signed-in server and back, no byte of the password ever goes
// to the page, and the page going away hangs up on the server (KTD19).
func TestRelayOffersNoneThenPipes(t *testing.T) {
	const pw = "Relay-Secret-5"
	srv := rfbtest.New(t, pw, 2)
	conn, err := signIn(t, srv, VNCAuth{Security: "none", Password: pw})
	if err != nil {
		t.Fatal(err)
	}
	page := rfbtest.NewPage()
	done := make(chan error, 1)
	go func() { done <- Relay(page, conn, OfferNone) }()

	if got := page.Read(t, 0, 12); string(got) != "RFB 003.008\n" {
		t.Fatalf("version %q", got)
	}
	page.Feed([]byte("RFB 003.008\n"))
	if got := page.Read(t, 12, 14); !bytes.Equal(got, []byte{1, 1}) {
		t.Fatalf("security types %v, want only None", got)
	}
	page.Feed([]byte{1})
	if got := page.Read(t, 14, 18); !bytes.Equal(got, []byte{0, 0, 0, 0}) {
		t.Fatalf("security result %v", got)
	}
	page.Feed([]byte{1}) // ClientInit, shared
	n := 18 + len(rfbtest.ServerInit)
	if got := page.Read(t, 18, n); !bytes.Equal(got, rfbtest.ServerInit) {
		t.Fatalf("ServerInit %v", got)
	}
	page.Feed([]byte("key event"))
	if got := page.Read(t, n, n+9); string(got) != "key event" {
		t.Fatalf("echo %q", got)
	}
	if bytes.Contains(page.Sent(), []byte(pw)) || bytes.Contains(page.Sent(), []byte(pw[:8])) {
		t.Fatal("the password reached the page")
	}

	page.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay outlived the page")
	}
	waitFor(t, func() bool { return slices.Contains(srv.Log(), "closed") })
}

// A page that does not speak RFB 3.8 gets nothing from the server.
func TestRelayEndsOnABadPageGreeting(t *testing.T) {
	srv := rfbtest.New(t, "pw", 2)
	conn, err := signIn(t, srv, VNCAuth{Security: "none", Password: "pw"})
	if err != nil {
		t.Fatal(err)
	}
	page := rfbtest.NewPage()
	page.Feed([]byte("HTTP/1.1 200"))
	if err := Relay(page, conn, OfferNone); err == nil {
		t.Fatal("a bad greeting was accepted")
	}
	waitFor(t, func() bool { return slices.Contains(srv.Log(), "closed") })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}
