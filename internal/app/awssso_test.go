package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"hostbay/internal/apperr"
)

// awsSSOMarker stands in for a SAMLResponse; no reply, error or log line may
// carry it.
const awsSSOMarker = "PHNhbWxwOlJlc3BvbnNlIHNlY3JldA-marker"

// useAWSSSOPort points the sign-in listener at a free loopback port and
// returns its address.
func useAWSSSOPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	old := awsSSOAddr
	awsSSOAddr = addr
	t.Cleanup(func() { awsSSOAddr = old })
	return addr
}

// awsSSOPost posts form to addr, naming host as its Host and origin as its
// Origin when set, and returns the status and reply.
func awsSSOPost(t *testing.T, addr, host, origin string, form url.Values) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return awsSSODo(t, req)
}

func awsSSODo(t *testing.T, req *http.Request) (int, string) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// awsSSOWaitAsync runs l.wait and returns where its result arrives.
func awsSSOWaitAsync(ctx context.Context, l *awsSSOListener) chan [2]any {
	out := make(chan [2]any, 1)
	go func() {
		resp, err := l.wait(ctx)
		out <- [2]any{resp, err}
	}()
	return out
}

func awsSSOResult(t *testing.T, out chan [2]any) (string, error) {
	t.Helper()
	select {
	case r := <-out:
		err, _ := r[1].(error)
		return r[0].(string), err
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return")
		return "", nil
	}
}

func assertAWSSSOClosed(t *testing.T, addr string) {
	t.Helper()
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Fatalf("%s still accepts connections after the sign-in ended", addr)
	}
}

func TestAWSSSOAcceptsPostFromIdPAndCloses(t *testing.T) {
	addr := useAWSSSOPort(t)
	l, err := listenAWSSSO()
	if err != nil {
		t.Fatal(err)
	}
	out := awsSSOWaitAsync(context.Background(), l)
	code, body := awsSSOPost(t, addr, "", "https://idp.example.com", url.Values{"SAMLResponse": {awsSSOMarker}, "RelayState": {"x"}})
	if code != http.StatusOK || !strings.Contains(body, "Return to Hostbay") {
		t.Fatalf("post = %d %q, want 200 with the return page", code, body)
	}
	resp, err := awsSSOResult(t, out)
	if err != nil || resp != awsSSOMarker {
		t.Fatalf("wait = %q, %v; want the posted response", resp, err)
	}
	assertAWSSSOClosed(t, addr)
}

func TestAWSSSORefusedRequestsKeepWaiting(t *testing.T) {
	valid := url.Values{"SAMLResponse": {awsSSOMarker}}
	cases := []struct {
		name string
		req  func(addr string) *http.Request
		want int
		// direct serves the request through the handler, not a socket: a
		// server that stops reading a large body may reset the connection
		// before the client reads the 413.
		direct bool
	}{
		{"GET", func(addr string) *http.Request {
			r, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/?SAMLResponse="+awsSSOMarker, nil)
			return r
		}, http.StatusMethodNotAllowed, false},
		{"POST without SAMLResponse", func(addr string) *http.Request {
			r, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/", strings.NewReader("RelayState=x"))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}, http.StatusBadRequest, false},
		{"POST with an empty SAMLResponse", func(addr string) *http.Request {
			r, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/", strings.NewReader("SAMLResponse="))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}, http.StatusBadRequest, false},
		{"POST over 256 KB", func(addr string) *http.Request {
			big := "SAMLResponse=" + strings.Repeat(awsSSOMarker, (awsSSOMaxBody/len(awsSSOMarker))+1)
			r, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/", strings.NewReader(big))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}, http.StatusRequestEntityTooLarge, true},
		{"POST with a broken form", func(addr string) *http.Request {
			r, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/", strings.NewReader("SAMLResponse=%zz"+awsSSOMarker))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}, http.StatusBadRequest, false},
		{"POST naming another Host", func(addr string) *http.Request {
			r, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/", strings.NewReader(valid.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Host = "localhost" + addr[strings.LastIndex(addr, ":"):]
			return r
		}, http.StatusForbidden, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logs bytes.Buffer
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			addr := useAWSSSOPort(t)
			l, err := listenAWSSSO()
			if err != nil {
				t.Fatal(err)
			}
			out := awsSSOWaitAsync(context.Background(), l)
			var code int
			var body string
			if c.direct {
				w := httptest.NewRecorder()
				l.ServeHTTP(w, c.req(addr))
				code, body = w.Code, w.Body.String()
			} else {
				code, body = awsSSODo(t, c.req(addr))
			}
			if code != c.want {
				t.Fatalf("status = %d %q, want %d", code, body, c.want)
			}
			if strings.Contains(body, awsSSOMarker) {
				t.Fatalf("the refusal carries the response: %q", body)
			}
			select {
			case r := <-out:
				t.Fatalf("the refused request ended the sign-in: %v", r)
			default:
			}
			if code, _ := awsSSOPost(t, addr, "", "", url.Values{"SAMLResponse": {"good"}}); code != http.StatusOK {
				t.Fatalf("a valid post after the refused one = %d, want 200", code)
			}
			if resp, err := awsSSOResult(t, out); err != nil || resp != "good" {
				t.Fatalf("wait = %q, %v; want the valid post's response", resp, err)
			}
			if strings.Contains(logs.String(), awsSSOMarker) {
				t.Fatalf("a log line carries the response: %q", logs.String())
			}
		})
	}
}

func TestAWSSSOSecondPostRefused(t *testing.T) {
	addr := useAWSSSOPort(t)
	l, err := listenAWSSSO()
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := awsSSOPost(t, addr, "", "", url.Values{"SAMLResponse": {"first"}}); code != http.StatusOK {
		t.Fatalf("first post = %d, want 200", code)
	}
	code, body := awsSSOPost(t, addr, "", "", url.Values{"SAMLResponse": {awsSSOMarker}})
	if code != http.StatusConflict || strings.Contains(body, awsSSOMarker) {
		t.Fatalf("second post = %d %q, want 409 without the response", code, body)
	}
	resp, err := l.wait(context.Background())
	if err != nil || resp != "first" {
		t.Fatalf("wait = %q, %v; want the first post's response", resp, err)
	}
	assertAWSSSOClosed(t, addr)
}

func TestAWSSSOWaitEnds(t *testing.T) {
	old := awsSSOWait
	t.Cleanup(func() { awsSSOWait = old })
	cases := []struct {
		name   string
		wait   time.Duration
		cancel bool
		check  func(t *testing.T, err error)
	}{
		{"timeout", 50 * time.Millisecond, false, func(t *testing.T, err error) {
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Code != apperr.VPNFailed || ae.Details["reason"] != awsSSOReasonTimeout || !strings.Contains(ae.Message, "not completed") {
				t.Fatalf("err = %#v, want VPNFailed %s saying the sign-in was not completed", err, awsSSOReasonTimeout)
			}
		}},
		{"disconnect", time.Minute, true, func(t *testing.T, err error) {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			awsSSOWait = c.wait
			addr := useAWSSSOPort(t)
			l, err := listenAWSSSO()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.cancel {
				time.AfterFunc(50*time.Millisecond, cancel)
			}
			// A refused post carrying a response must not reach the error.
			awsSSOPost(t, addr, "localhost", "", url.Values{"SAMLResponse": {awsSSOMarker}})
			resp, err := l.wait(ctx)
			if resp != "" {
				t.Fatalf("wait returned a response %q", resp)
			}
			c.check(t, err)
			if strings.Contains(err.Error(), awsSSOMarker) {
				t.Fatalf("the error carries the response: %v", err)
			}
			assertAWSSSOClosed(t, addr)
			l.close() // a second close is harmless
		})
	}
}

func TestAWSSSOLockHeld(t *testing.T) {
	release, err := takeAWSSSOLock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	_, err = takeAWSSSOLock()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.VPNFailed || ae.Details["reason"] != awsSSOReasonBusy || !strings.Contains(ae.Message, "in progress") {
		t.Fatalf("second take = %#v, want VPNFailed %s saying a sign-in is in progress", err, awsSSOReasonBusy)
	}
	release()
	release() // releasing twice does not unlock a later holder
	again, err := takeAWSSSOLock()
	if err != nil {
		t.Fatalf("take after release: %v", err)
	}
	release()
	if _, err := takeAWSSSOLock(); err == nil {
		t.Fatal("an earlier release freed the lock a later taker holds")
	}
	again()
}

func TestAWSSSOPortBusyNamesHolder(t *testing.T) {
	addr := useAWSSSOPort(t)
	held, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	l, err := listenAWSSSO()
	if err == nil {
		l.close()
		t.Fatal("listen succeeded on a held port")
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.VPNFailed || ae.Details["reason"] != awsSSOReasonPort {
		t.Fatalf("err = %#v, want VPNFailed %s", err, awsSSOReasonPort)
	}
	port := addr[strings.LastIndex(addr, ":")+1:]
	if !strings.Contains(ae.Message, "Port "+port+" is in use by ") {
		t.Fatalf("message = %q, want it to say the port is in use", ae.Message)
	}
	_, lsofErr := exec.LookPath("lsof")
	if runtime.GOOS != "windows" && lsofErr != nil {
		return // no lookup here, so no name to check
	}
	if exe := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe"); !strings.Contains(ae.Message, " by "+exe+",") {
		t.Fatalf("message = %q, want it to name %s", ae.Message, exe)
	}
}
