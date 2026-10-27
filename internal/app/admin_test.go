package app

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"hostbay/internal/sftp/sftptest"
)

func TestAdminServiceRunsOnTheSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	s := sftptest.New(t, "pw")
	f, l := newFilesSession(t, s)
	a := NewAdminService(f.mgr, nil)
	ctx := context.Background()

	// The test server's sudo refuses -n, so nginx -t never runs.
	r, err := a.NginxTest(ctx, l.ID())
	if err != nil || r.Code == nil || *r.Code == 0 || !strings.Contains(r.Stdout, "password is required") {
		t.Fatalf("NginxTest = %+v, %v", r, err)
	}
	if _, err := a.SystemdAction(ctx, l.ID(), "nginx;reboot", "restart"); err == nil {
		t.Error("unit with ; accepted")
	}
	if got := s.Commands(); len(got) != 1 || got[0] != "sudo -n nginx -t 2>&1" {
		t.Errorf("commands = %q", got)
	}
	if _, err := a.DockerPs(ctx, "no-such-session"); err == nil {
		t.Error("unknown session accepted")
	}
}
