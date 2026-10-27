//go:build integration

package sync

import (
	"context"
	"os"
	"strconv"
	"testing"
)

// Run against a real share:
//
//	HOSTBAY_SMB_HOST=127.0.0.1 HOSTBAY_SMB_SHARE=sync HOSTBAY_SMB_USER=u HOSTBAY_SMB_PASSWORD=p \
//	go test -tags integration -run SMB ./internal/sync/
func TestSMBStoresConverge(t *testing.T) {
	host := os.Getenv("HOSTBAY_SMB_HOST")
	if host == "" {
		t.Skip("HOSTBAY_SMB_HOST not set")
	}
	port, _ := strconv.Atoi(os.Getenv("HOSTBAY_SMB_PORT"))
	target := Target{Kind: "smb", Host: host, Port: port, Share: os.Getenv("HOSTBAY_SMB_SHARE"),
		User: os.Getenv("HOSTBAY_SMB_USER"), Domain: os.Getenv("HOSTBAY_SMB_DOMAIN"), Path: "hostbay-it-" + strconv.Itoa(os.Getpid())}
	connect := func() *device {
		b, err := Connect(context.Background(), target, os.Getenv("HOSTBAY_SMB_PASSWORD"), Deps{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close() })
		return newDevice(t, b)
	}
	a, b := connect(), connect()
	ida := a.addHost(t, "from-a", "")
	a.sync(t)
	b.sync(t)
	idb := b.addHost(t, "from-b", "")
	b.sync(t)
	a.sync(t)
	for _, d := range []*device{a, b} {
		if got := aliases(t, d); got[ida] != "from-a" || got[idb] != "from-b" {
			t.Fatalf("got %v", got)
		}
	}
}
