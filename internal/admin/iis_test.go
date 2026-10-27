package admin

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// fakePowerShell prints out and saves its arguments to the returned file.
func fakePowerShell(t *testing.T, out string) string {
	t.Helper()
	args := filepath.Join(t.TempDir(), "args")
	fakeBin(t, "powershell", `printf '%s\n' "$@" > '`+args+`'
cat <<'EOF'
`+out+`
EOF
`)
	return args
}

// decodedScript reads the -EncodedCommand argument back to its script.
func decodedScript(t *testing.T, argsFile string) string {
	t.Helper()
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(args) != 4 || !slices.Equal(args[:3], []string{"-NoProfile", "-NonInteractive", "-EncodedCommand"}) {
		t.Fatalf("args = %q", args)
	}
	raw, err := base64.StdEncoding.DecodeString(args[3])
	if err != nil {
		t.Fatal(err)
	}
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}
	return string(utf16.Decode(u))
}

func TestIisSiteAction(t *testing.T) {
	args := fakePowerShell(t, "")
	srv, c := testServer(t)
	if _, err := IisSiteAction(context.Background(), c, "Default Web Site", "restart"); err != nil {
		t.Fatal(err)
	}
	want := psPrelude + "Import-Module WebAdministration; Stop-Website -Name 'Default Web Site'; Start-Website -Name 'Default Web Site'"
	if got := decodedScript(t, args); got != want {
		t.Errorf("script = %q, want %q", got, want)
	}
	// Only base64 reaches the remote shell, so cmd.exe, PowerShell and sh
	// all pass it through untouched.
	cmd := srv.Commands()[0]
	b64, ok := strings.CutPrefix(cmd, "powershell -NoProfile -NonInteractive -EncodedCommand ")
	if !ok || strings.Trim(b64, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/=") != "" {
		t.Errorf("command = %q", cmd)
	}
}

func TestIisPoolAction(t *testing.T) {
	args := fakePowerShell(t, "")
	_, c := testServer(t)
	if _, err := IisPoolAction(context.Background(), c, "DefaultAppPool", "recycle"); err != nil {
		t.Fatal(err)
	}
	if got, want := decodedScript(t, args), psPrelude+"Import-Module WebAdministration; Restart-WebAppPool -Name 'DefaultAppPool'"; got != want {
		t.Errorf("script = %q, want %q", got, want)
	}
}

func TestIisRejectsBeforeRunning(t *testing.T) {
	srv, c := testServer(t)
	ctx := context.Background()
	if _, err := IisSiteAction(ctx, c, "site'; Stop-Computer; '", "start"); err == nil {
		t.Error("site name with a quote accepted")
	}
	if _, err := IisSiteAction(ctx, c, "site", "delete"); err == nil {
		t.Error("site action delete accepted")
	}
	if _, err := IisPoolAction(ctx, c, `pool"$(x)`, "start"); err == nil {
		t.Error("pool name with a quote accepted")
	}
	if _, err := IisPoolAction(ctx, c, "pool", "restart"); err == nil {
		t.Error("pool action restart accepted (recycle is the verb)")
	}
	if got := srv.Commands(); len(got) != 0 {
		t.Fatalf("commands ran: %q", got)
	}
}

func TestIisLists(t *testing.T) {
	ctx := context.Background()
	fakePowerShell(t, `{"name":"Default Web Site","id":1,"state":"Started","physicalPath":"C:\\inetpub\\wwwroot","bindings":"*:80:","appPool":"DefaultAppPool"}`)
	_, c := testServer(t)
	sites, err := IisSites(ctx, c)
	want := IisSite{Name: "Default Web Site", ID: 1, State: "Started", PhysicalPath: `C:\inetpub\wwwroot`, Bindings: "*:80:", AppPool: "DefaultAppPool"}
	if err != nil || len(sites) != 1 || sites[0] != want {
		t.Errorf("one site = %+v, %v", sites, err)
	}

	fakePowerShell(t, `[{"name":"A","state":"Started","runtime":"v4.0"},{"name":"B","state":"Stopped","runtime":""}]`)
	_, c = testServer(t)
	pools, err := IisPools(ctx, c)
	if err != nil || !slices.Equal(pools, []IisPool{{"A", "Started", "v4.0"}, {"B", "Stopped", ""}}) {
		t.Errorf("pools = %+v, %v", pools, err)
	}
}

func TestWindowsDashboard(t *testing.T) {
	fakePowerShell(t, "OS::Microsoft Windows Server 2022 Standard 10.0.20348\r\nUPTIME::49h 3m\r\nCPU::Intel Xeon\r\nMEM::3.1 / 8 GB\r\nDISK::40 / 127 GB\r\nIIS::True")
	_, c := testServer(t)
	got, err := WindowsDashboard(context.Background(), c)
	want := WinDashboard{OS: "Microsoft Windows Server 2022 Standard 10.0.20348", Uptime: "49h 3m", CPU: "Intel Xeon", Memory: "3.1 / 8 GB", Disk: "40 / 127 GB", IisInstalled: true}
	if err != nil || got != want {
		t.Errorf("dashboard = %+v, %v", got, err)
	}
}
