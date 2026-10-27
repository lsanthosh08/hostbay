package app

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

type appendCall struct {
	host    string
	port    int
	keyType string
	key     []byte
}

// newTestTrust returns a registry with a controllable clock and a recording
// known_hosts appender.
func newTestTrust() (*HostKeyTrust, *time.Time, *[]appendCall) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	var calls []appendCall
	t := NewHostKeyTrust(func(host string, port int, keyType string, key []byte) error {
		calls = append(calls, appendCall{host, port, keyType, key})
		return nil
	})
	t.now = func() time.Time { return now }
	return t, &now, &calls
}

func requestID(t *testing.T, tr *HostKeyTrust, host string) string {
	t.Helper()
	e := tr.Request(host, 22, "ssh-ed25519", []byte("captured-key"), "SHA256:abc")
	id := e.Details["requestId"]
	if id == "" {
		t.Fatalf("Request returned no requestId: %+v", e)
	}
	return id
}

func TestTrustRejectsUnknownIDsWithoutAppending(t *testing.T) {
	tr, _, calls := newTestTrust()
	requestID(t, tr, "fe80::1")

	// Anything the UI might send instead of an issued ID: junk, a host name,
	// or key text.
	for _, id := range []string{"", "nope", "fe80::1", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA"} {
		if err := tr.Answer(id, true); err == nil {
			t.Errorf("Answer(%q, true) = nil, want error", id)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("AppendKnownHost called %d times, want 0", len(*calls))
	}
}

func TestTrustRejectsExpiredID(t *testing.T) {
	tr, now, calls := newTestTrust()
	id := requestID(t, tr, "db.internal")

	*now = now.Add(pendingTrustTTL + time.Second)
	if err := tr.Answer(id, true); err == nil {
		t.Fatal("Answer on expired ID = nil, want error")
	}
	if len(*calls) != 0 {
		t.Fatalf("AppendKnownHost called %d times, want 0", len(*calls))
	}
}

func TestTrustAcceptAppendsStoredKeyOnce(t *testing.T) {
	tr, _, calls := newTestTrust()
	id := requestID(t, tr, "fe80::1")

	if err := tr.Answer(id, true); err != nil {
		t.Fatalf("Answer = %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("AppendKnownHost called %d times, want 1", len(*calls))
	}
	c := (*calls)[0]
	if c.host != "fe80::1" || c.port != 22 || c.keyType != "ssh-ed25519" || !bytes.Equal(c.key, []byte("captured-key")) {
		t.Fatalf("appended %+v, want the captured fe80::1:22 ssh-ed25519 key", c)
	}

	// Single use: the same ID cannot be replayed.
	if err := tr.Answer(id, true); err == nil {
		t.Fatal("second Answer = nil, want error")
	}
	if len(*calls) != 1 {
		t.Fatalf("AppendKnownHost called %d times after replay, want 1", len(*calls))
	}
}

func TestTrustDeclineDropsRequest(t *testing.T) {
	tr, _, calls := newTestTrust()
	id := requestID(t, tr, "web1")

	if err := tr.Answer(id, false); err != nil {
		t.Fatalf("Answer(decline) = %v", err)
	}
	if err := tr.Answer(id, true); err == nil {
		t.Fatal("Answer after decline = nil, want error")
	}
	if len(*calls) != 0 {
		t.Fatalf("AppendKnownHost called %d times, want 0", len(*calls))
	}
}

func TestTrustRequestErrorReachesUIWithIPv6HostIntact(t *testing.T) {
	tr, _, _ := newTestTrust()
	err := tr.Request("fe80::1", 2222, "ssh-ed25519", []byte("k"), "SHA256:xyz")

	var got struct {
		Code    string            `json:"code"`
		Message string            `json:"message"`
		Details map[string]string `json:"details"`
	}
	if jerr := json.Unmarshal(MarshalError(err), &got); jerr != nil {
		t.Fatal(jerr)
	}
	d := got.Details
	if got.Code != "HostKeyUnknown" || d["host"] != "fe80::1" || d["port"] != "2222" ||
		d["keyType"] != "ssh-ed25519" || d["fingerprint"] != "SHA256:xyz" || d["requestId"] == "" {
		t.Fatalf("got %+v", got)
	}
	if _, leaked := d["key"]; leaked {
		t.Fatal("key bytes must not be sent to the UI")
	}
}

func TestTrustExpiredRequestIsSweptByLaterRequest(t *testing.T) {
	tr, now, _ := newTestTrust()
	old := requestID(t, tr, "web1")
	*now = now.Add(pendingTrustTTL + time.Second)
	requestID(t, tr, "web2")
	if _, ok := tr.pending[old]; ok || len(tr.pending) != 1 {
		t.Fatalf("pending = %d entries, expired one kept = %v", len(tr.pending), ok)
	}
}

func TestRemovalRequestExpires(t *testing.T) {
	tr, now, _ := newTestTrust()
	id := tr.RemovalRequest("db.internal", 22)
	if p, ok := tr.removal(id); !ok || p.host != "db.internal" || p.port != 22 {
		t.Fatalf("removal = %+v, %v", p, ok)
	}
	if err := tr.Answer(id, true); err == nil {
		t.Fatal("a removal ID answered a trust prompt")
	}
	*now = now.Add(pendingTrustTTL + time.Second)
	if _, ok := tr.removal(id); ok {
		t.Fatal("expired removal request still usable")
	}
}
