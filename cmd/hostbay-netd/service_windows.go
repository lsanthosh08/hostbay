package main

import (
	"context"

	"golang.org/x/sys/windows/svc"
)

// runService runs serve under the Windows service manager when it started
// the process, stopping it on a stop or shutdown request.
func runService(serve func(context.Context) error) (bool, error) {
	if ok, err := svc.IsWindowsService(); err != nil || !ok {
		return false, err
	}
	var serr error
	err := svc.Run("hostbay-netd", handler(func(ctx context.Context) { serr = serve(ctx) }))
	if err != nil {
		return true, err
	}
	return true, serr
}

type handler func(context.Context)

func (h handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h(ctx); close(done) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		case <-done: // serve failed on its own
			cancel()
			return false, 1
		}
	}
}
