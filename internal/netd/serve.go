package netd

import (
	"context"
	"errors"
	"log"
	"net"
	"path/filepath"
)

// TunnelEnv is what the helper's tunnel runner works with.
type TunnelEnv struct {
	Dir   string // the helper's root-owned directory, holding the VPN clients
	User  string // the installing user: a uid, or a SID on Windows
	Owner int    // who must own the VPN client copies: root outside tests
}

// NewTunnels builds the runner for system-wide tunnels (U12). cmd/hostbay-netd
// sets it: this package links none of the tunnel code, since Hostbay links
// this package too. Unset, the helper answers tunnel requests
// CodeUnsupported.
var NewTunnels func(TunnelEnv) Tunnels

// Serve runs the installed helper until ctx ends: it sweeps whatever a
// previous run left journaled, then serves the paired Hostbay, and sweeps
// again on the way out so stopping or uninstalling the helper leaves no
// Hostbay route behind.
func Serve(ctx context.Context) error {
	dir := stateDir()
	cfg, err := loadConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		return err
	}
	j, err := OpenJournal(filepath.Join(dir, "journal.json"))
	if err != nil {
		return err
	}
	s := &Server{
		Table:   newTable(),
		Journal: j,
		Auth: func(c net.Conn) error {
			p, err := peerOf(c)
			if err != nil {
				return &Error{CodeDenied, "Cannot identify the caller: " + err.Error()}
			}
			return authorize(p, cfg)
		},
	}
	if NewTunnels != nil {
		s.Tunnels = NewTunnels(TunnelEnv{Dir: dir, User: cfg.UID})
	}
	s.Sweep()

	ln, err := listen(cfg)
	if err != nil {
		return err
	}
	go func() {
		if err := watchRoutes(ctx, s.Notify); err != nil && ctx.Err() == nil {
			log.Printf("hostbay-netd: route events stopped: %v", err)
		}
	}()
	context.AfterFunc(ctx, func() { ln.Close() })
	err = s.Serve(ln)
	s.Stop()
	if ctx.Err() != nil && errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
