package netd

import (
	"context"
	"errors"
	"log"
	"net"
	"path/filepath"
)

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
