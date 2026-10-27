package admin

import (
	"context"

	"golang.org/x/crypto/ssh"
)

// Neither takes input.

func NginxTest(ctx context.Context, c *ssh.Client) (Result, error) {
	return Exec(ctx, c, "sudo -n nginx -t 2>&1")
}

func NginxReload(ctx context.Context, c *ssh.Client) (Result, error) {
	return Exec(ctx, c, "sudo -n nginx -t && sudo -n systemctl reload nginx 2>&1")
}
