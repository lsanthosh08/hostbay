// Command hostbay-netd is Hostbay's privileged helper (KTD8). Packaging puts
// it beside the hostbay executable; Hostbay's install runs it once as root
// (one administrator prompt), and it copies itself into a root-owned
// directory and registers itself as a launchd daemon, systemd unit or
// Windows service, which runs "hostbay-netd serve". See internal/netd.
//
//	hostbay-netd serve            run the helper (the daemon does this)
//	hostbay-netd install [<uid>]  install and pair with the hostbay beside it (as root)
//	hostbay-netd uninstall        stop and remove the installed helper (as root)
//
// install takes the installing user's uid on macOS and SID on Windows; on
// Linux pkexec supplies it.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"hostbay/internal/netd"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "hostbay-netd:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: hostbay-netd serve | install [<user>] | uninstall")
	}
	switch args[0] {
	case "serve":
		if ok, err := runService(netd.Serve); ok {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return netd.Serve(ctx)
	case "install":
		user := ""
		if len(args) > 1 {
			user = args[1]
		}
		return netd.SetUp(user)
	case "uninstall":
		return netd.TearDown()
	}
	return fmt.Errorf("unknown command %q", args[0])
}
