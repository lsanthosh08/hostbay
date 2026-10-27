package container

import (
	"context"

	"hostbay/internal/admin"
	"hostbay/internal/sshx"
	"hostbay/internal/terminal"
)

// SSHRunner runs engine commands on a connected host, over its lease's
// shared connection (KTD5). A dropped connection is rebuilt first.
type SSHRunner struct{ Lease *sshx.Lease }

func (r SSHRunner) Run(ctx context.Context, argv []string, stdin []byte) (admin.Result, error) {
	cl, err := r.Lease.Live(ctx)
	if err != nil {
		return admin.Result{}, err
	}
	return admin.ExecStdin(ctx, cl.Client, Command(argv), stdin)
}

func (r SSHRunner) PTY(ctx context.Context, argv []string, cols, rows int) (terminal.Transport, error) {
	return terminal.OpenSSHCommand(ctx, r.Lease, cols, rows, Command(argv))
}
