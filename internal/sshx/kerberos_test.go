//go:build !windows && cgo

package sshx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/apperr"
)

// fakeGSSServer accepts the Kerberos mechanism so the client starts its
// context; it never sees a token here.
type fakeGSSServer struct{}

func (fakeGSSServer) AcceptSecContext([]byte) ([]byte, string, bool, error) {
	return nil, "", false, errors.New("unexpected token")
}
func (fakeGSSServer) VerifyMIC([]byte, []byte) error { return errors.New("unexpected MIC") }
func (fakeGSSServer) DeleteSecContext() error        { return nil }

// With no ticket, a Kerberos login asks for kinit and sends nothing else.
// The full exchange is checked by hand against a test KDC (U19).
func TestKerberosWithoutTicketAsksForKinit(t *testing.T) {
	if loadGSS() != nil {
		t.Skip("no system GSSAPI library")
	}
	dir := t.TempDir()
	t.Setenv("KRB5CCNAME", "FILE:"+filepath.Join(dir, "none"))
	conf := filepath.Join(dir, "krb5.conf")
	os.WriteFile(conf, []byte("[libdefaults]\n default_realm = TEST.INVALID\n dns_lookup_kdc = false\n"), 0o600)
	t.Setenv("KRB5_CONFIG", conf)

	s := newTestServer(t, serverOpts{gss: &ssh.GSSAPIWithMICConfig{
		AllowLogin: func(ssh.ConnMetadata, string) (*ssh.Permissions, error) { return nil, nil },
		Server:     fakeGSSServer{},
	}})
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}
	_, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{{Host: s.host, Port: s.port, User: "u", Auth: Auth{Kerberos: true}}}})
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.SecretNeeded || ae.Details["kind"] != "kerberos" {
		t.Fatalf("err = %v, want SecretNeeded kind kerberos", err)
	}
}
