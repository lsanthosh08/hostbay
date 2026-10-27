package admin

import (
	"context"
	"slices"
	"testing"
)

func TestSystemd(t *testing.T) {
	fakeBin(t, "systemctl", `echo 'nginx.service   loaded active running A high performance web server'
echo ''
echo 'ssh.service loaded failed failed OpenBSD Secure Shell server'
`)
	srv, c := testServer(t)
	ctx := context.Background()

	units, err := SystemdList(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	want := []SystemdUnit{
		{"nginx.service", "loaded", "active", "running", "A high performance web server"},
		{"ssh.service", "loaded", "failed", "failed", "OpenBSD Secure Shell server"},
	}
	if !slices.Equal(units, want) {
		t.Errorf("units = %+v", units)
	}

	// sudo is the test server's fake; -n makes it refuse, which is still a
	// result the UI shows, not an error.
	if _, err := SystemdAction(ctx, c, "getty@tty1.service", "restart"); err != nil {
		t.Fatal(err)
	}
	cmds := srv.Commands()
	if want := "sudo -n systemctl restart 'getty@tty1.service' 2>&1"; cmds[len(cmds)-1] != want {
		t.Errorf("action = %q, want %q", cmds[len(cmds)-1], want)
	}
}

func TestSystemdRejectsBeforeRunning(t *testing.T) {
	srv, c := testServer(t)
	ctx := context.Background()
	if _, err := SystemdAction(ctx, c, "nginx;reboot", "restart"); err == nil {
		t.Error("unit with ; accepted")
	}
	if _, err := SystemdAction(ctx, c, "nginx", "mask"); err == nil {
		t.Error("action mask accepted")
	}
	for _, u := range hostile {
		if _, err := SystemdAction(ctx, c, u, "stop"); err == nil {
			t.Errorf("unit %q accepted", u)
		}
	}
	if got := srv.Commands(); len(got) != 0 {
		t.Fatalf("commands ran: %q", got)
	}
}

func TestDashboardInfo(t *testing.T) {
	fakeBin(t, "nginx", "exit 0\n")
	_, c := testServer(t)
	info, err := Dashboard(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if info.OS == "" || info.Uptime == "" || info.Disk == "" || !info.NginxInstalled {
		t.Errorf("info = %+v", info)
	}
}
