package sshx

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Algorithms are SSH algorithms a host opts in to for a server that offers
// nothing newer (R52). Each list is offered after x/crypto/ssh's supported,
// secure algorithms, so a server that also speaks a modern one still gets
// it. A host with no list keeps the library's defaults.
type Algorithms struct {
	KeyExchanges []string
	Ciphers      []string
	MACs         []string
	HostKeys     []string
}

// Validate refuses a name x/crypto/ssh does not implement for its list;
// there is no forked crypto to fall back on.
func (a Algorithms) Validate() error {
	sup, insec := ssh.SupportedAlgorithms(), ssh.InsecureAlgorithms()
	for _, l := range []struct {
		what         string
		names, known []string
	}{
		{"key exchange", a.KeyExchanges, slices.Concat(sup.KeyExchanges, insec.KeyExchanges)},
		{"cipher", a.Ciphers, slices.Concat(sup.Ciphers, insec.Ciphers)},
		{"MAC", a.MACs, slices.Concat(sup.MACs, insec.MACs)},
		{"host key algorithm", a.HostKeys, slices.Concat(sup.HostKeys, insec.HostKeys)},
	} {
		for _, n := range l.names {
			if !slices.Contains(l.known, n) {
				return fmt.Errorf("unknown %s %q", l.what, n)
			}
		}
	}
	return nil
}

// Insecure reports whether any of a's algorithms has known weaknesses. A
// synced change that adds one waits for local confirmation (KTD22).
func (a Algorithms) Insecure() bool {
	in := ssh.InsecureAlgorithms()
	weak := slices.Concat(in.KeyExchanges, in.Ciphers, in.MACs, in.HostKeys)
	return slices.ContainsFunc(slices.Concat(a.KeyExchanges, a.Ciphers, a.MACs, a.HostKeys), func(n string) bool {
		return slices.Contains(weak, n)
	})
}

// apply sets cfg's lists from a; the host key list is the known_hosts-led
// one hostKeyCallback chose, with a's extras after it (AE10).
func (a Algorithms) apply(cfg *ssh.ClientConfig) {
	sup := ssh.SupportedAlgorithms()
	extend := func(base, extra []string) []string {
		if len(extra) == 0 {
			return nil // the library's defaults
		}
		out := slices.Clone(base)
		for _, n := range extra {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
		return out
	}
	cfg.KeyExchanges = extend(sup.KeyExchanges, a.KeyExchanges)
	cfg.Ciphers = extend(sup.Ciphers, a.Ciphers)
	cfg.MACs = extend(sup.MACs, a.MACs)
	// x/crypto's CBC ciphers cannot encrypt-then-MAC, so a host that opts
	// in to one prefers the plain HMACs; a modern cipher is unaffected.
	if slices.ContainsFunc(a.Ciphers, func(c string) bool { return strings.HasSuffix(c, "-cbc") }) {
		macs := cfg.MACs
		if macs == nil {
			macs = sup.MACs
		}
		etm := func(m string) bool { return strings.HasSuffix(m, "-etm@openssh.com") }
		plain := slices.DeleteFunc(slices.Clone(macs), etm)
		cfg.MACs = append(plain, slices.DeleteFunc(slices.Clone(macs), func(m string) bool { return !etm(m) })...)
	}
	if hk := extend(cfg.HostKeyAlgorithms, a.HostKeys); hk != nil {
		cfg.HostKeyAlgorithms = hk
	}
}
