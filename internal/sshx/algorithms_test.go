package sshx

import (
	"context"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// A server that offers only diffie-hellman-group1-sha1 is reached only when
// the host opts in, and otherwise fails saying how to opt in (R52).
func TestLegacyKeyExchangeOnlyWhenOptedIn(t *testing.T) {
	s := newTestServer(t, serverOpts{kex: []string{ssh.InsecureKeyExchangeDH1SHA1}})
	d := &Dialer{KnownHostsPath: writeKnownHosts(t, s.knownLine(t))}

	_, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{s.hop()}})
	if err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("err = %v, want a pointer to the legacy algorithm setting", err)
	}

	hop := s.hop()
	hop.Algorithms = Algorithms{KeyExchanges: []string{ssh.InsecureKeyExchangeDH1SHA1}}
	c, err := d.Dial(context.Background(), Target{Key: "k", Hops: []Hop{hop}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := run(t, c, "echo hi"); got != "hi\n" {
		t.Fatalf("exec = %q", got)
	}
}

// An extra host key type is offered after the known_hosts-derived ones, so
// a known ed25519 key still wins over an opted-in ssh-rsa (AE10).
func TestExtraHostKeyTypesComeAfterKnownHosts(t *testing.T) {
	cfg := &ssh.ClientConfig{HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoRSASHA512}}
	Algorithms{HostKeys: []string{ssh.KeyAlgoRSA, ssh.KeyAlgoED25519}}.apply(cfg)
	if want := []string{ssh.KeyAlgoED25519, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSA}; strings.Join(cfg.HostKeyAlgorithms, ",") != strings.Join(want, ",") {
		t.Fatalf("host key algorithms = %v, want %v", cfg.HostKeyAlgorithms, want)
	}
	if cfg.KeyExchanges != nil || cfg.Ciphers != nil || cfg.MACs != nil {
		t.Fatal("lists the host did not extend left their defaults")
	}
}

func TestAlgorithmsValidateAndInsecure(t *testing.T) {
	if err := (Algorithms{Ciphers: []string{"rot13"}}).Validate(); err == nil {
		t.Fatal("an unknown cipher was accepted")
	}
	legacy := Algorithms{KeyExchanges: []string{ssh.InsecureKeyExchangeDH1SHA1}, Ciphers: []string{ssh.InsecureCipherAES128CBC}}
	if err := legacy.Validate(); err != nil {
		t.Fatal(err)
	}
	if !legacy.Insecure() || (Algorithms{KeyExchanges: []string{ssh.KeyExchangeDH16SHA512}}).Insecure() {
		t.Fatal("Insecure misjudged")
	}
}

// x/crypto's CBC ciphers cannot encrypt-then-MAC, so opting in to one
// puts the plain HMACs first.
func TestCBCOptInPrefersPlainMACs(t *testing.T) {
	cfg := &ssh.ClientConfig{}
	Algorithms{Ciphers: []string{ssh.InsecureCipherAES128CBC}}.apply(cfg)
	if len(cfg.MACs) == 0 || strings.HasSuffix(cfg.MACs[0], "-etm@openssh.com") || !slices.Contains(cfg.MACs, ssh.HMACSHA256ETM) {
		t.Fatalf("MACs = %v", cfg.MACs)
	}
}
