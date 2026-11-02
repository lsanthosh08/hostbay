package sase

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const connected = "objc[1]: Class X is implemented in both\nStarting CLI with command vpnConnect\nNetwork:         lab\nStatus:          connected\nInternal IP:     10.255.0.9\n"

func fake(out string, err error) (quits *int) {
	n := 0
	quit = func(string) { n++ }
	run = func(context.Context, string, ...string) (string, error) { return out, err }
	return &n
}

func TestConnectQuitsTheMenuBarAppThenReadsTheStatus(t *testing.T) {
	n := fake(connected, nil)
	if err := Connect(context.Background(), "/x"); err != nil || *n != 1 {
		t.Fatalf("err %v, quits %d", err, *n)
	}
}

func TestConnectNamesWhyItFailed(t *testing.T) {
	fake("noise\nError: not logged in\n", errors.New("exit status 1"))
	if err := Connect(context.Background(), "/x"); err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err %v", err)
	}
	fake("Status:          disconnected\n", nil)
	if err := Connect(context.Background(), "/x"); err == nil || !strings.Contains(err.Error(), "disconnected") {
		t.Fatalf("a connect that left it disconnected: %v", err)
	}
}

func TestDisconnect(t *testing.T) {
	fake("Starting CLI with command vpnDisconnect\nNetwork lab disconnected\n", nil)
	if err := Disconnect(context.Background(), "/x"); err != nil {
		t.Fatal(err)
	}
}

func TestBinNeedsAnAppBundle(t *testing.T) {
	if Bin("/usr/bin/true") != "" || Bin("/Applications/Nope.app") != "" {
		t.Fatal("a path that is not an installed app bundle gave a CLI")
	}
}
