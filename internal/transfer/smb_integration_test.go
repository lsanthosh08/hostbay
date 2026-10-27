//go:build integration

package transfer_test

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"

	"hostbay/internal/transfer"
	"hostbay/internal/transfer/sourcetest"
)

// Run against a real share, which the suite leaves as it found it:
//
//	HOSTBAY_SMB_HOST=127.0.0.1 HOSTBAY_SMB_SHARE=sync HOSTBAY_SMB_USER=u HOSTBAY_SMB_PASSWORD=p \
//	go test -tags integration -run SMB ./internal/transfer/
func TestSMBPassesTheSourceSuite(t *testing.T) {
	host := os.Getenv("HOSTBAY_SMB_HOST")
	if host == "" {
		t.Skip("HOSTBAY_SMB_HOST not set")
	}
	port := os.Getenv("HOSTBAY_SMB_PORT")
	if port == "" {
		port = "445"
	}
	s, err := transfer.DialSMB(context.Background(), nil, net.JoinHostPort(host, port),
		os.Getenv("HOSTBAY_SMB_USER"), os.Getenv("HOSTBAY_SMB_PASSWORD"), os.Getenv("HOSTBAY_SMB_DOMAIN"))
	if err != nil {
		t.Fatal(err)
	}
	src := transfer.NewSMB(s)
	t.Cleanup(func() { src.Close() })
	ctx := context.Background()
	share := "/" + os.Getenv("HOSTBAY_SMB_SHARE")

	es, err := src.List(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range es {
		found = found || e.Path == share && e.IsDir
	}
	if !found {
		t.Fatalf("the root lists %+v, not the share %s", es, share)
	}

	dir := "hostbay-it-" + strconv.Itoa(os.Getpid())
	if err := src.Mkdir(ctx, share, dir); err != nil {
		t.Fatal(err)
	}
	sourcetest.RunWithOddName(t, src, share+"/"+dir, "it's [odd] & spaced.txt")
	if err := src.Remove(ctx, share+"/"+dir, true); err != nil {
		t.Fatal(err)
	}
}
