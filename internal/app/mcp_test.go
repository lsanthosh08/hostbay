package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"hostbay/internal/agentexec"
	"hostbay/internal/backup"
	"hostbay/internal/mcp/mcptest"
	"hostbay/internal/secrets"
	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
	"hostbay/internal/tlstrust"
	"hostbay/internal/vpn/vpntest"
)

const mcpSamplePassword = "Mcp-Sample-Pass-4417"

type mcpEnv struct {
	s       *MCPService
	srv     *sftptest.Server
	conns   *ConnService
	hosts   *HostService
	host    string // the saved SSH host with its password saved
	confirm bool
	copied  string
	dialogs int
}

func newMCPEnv(t *testing.T) *mcpEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	e := &mcpEnv{srv: sftptest.New(t, mcpSamplePassword), confirm: true}
	hs, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	kh := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(kh, []byte(e.srv.KnownHostsLine()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.conns = NewConnService(NewHostKeyTrust(sshx.NewKnownHostAppender(kh)), creds, sshx.NewManager(&sshx.Dialer{KnownHostsPath: kh, Timeout: 5 * time.Second}))
	e.hosts = hs
	h, p, _ := net.SplitHostPort(e.srv.Addr)
	port, _ := strconv.Atoi(p)
	pw, auth := mcpSamplePassword, "password"
	saved, err := hs.SaveHost(Host{Alias: "web", Hostname: h, Port: port, User: "u", AuthType: &auth, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	e.host = saved.ID
	ai := NewAIService(creds.st, creds.vault, creds, e.conns, tlstrust.New(t.TempDir()))
	ai.emit = func(string, any) {}
	e.s = NewMCPService(creds.st, e.conns, creds, ai)
	e.s.confirm = func(context.Context, string, string) bool { e.dialogs++; return e.confirm }
	e.s.gate = func(context.Context, string) error { return nil }
	e.s.copy = func(s string) bool { e.copied = s; return true }
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	free := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if err := e.s.SetPort(free); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.s.ServiceShutdown() })
	return e
}

func (e *mcpEnv) client(t *testing.T, token string) *sdk.ClientSession {
	t.Helper()
	st, _ := e.s.Status()
	cs, err := mcptest.HTTP(t.Context(), st.Endpoint, token, mcptest.V20260728)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func (e *mcpEnv) answer(t *testing.T, allow bool) agentexec.Pending {
	t.Helper()
	a := e.s.srv.Jobs.Approvals
	for range 400 {
		if l := a.List(); len(l) > 0 {
			a.Answer(l[0].ID, allow)
			return l[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no card showed")
	return agentexec.Pending{}
}

func mcpCall(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) (bool, string) {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return res.IsError, b.String()
}

// Turning the server on with the native dialog answering no starts no
// listener and saves nothing.
func TestMCPEnableNeedsTheDialog(t *testing.T) {
	e := newMCPEnv(t)
	e.confirm = false
	if err := e.s.SetEnabled(t.Context(), true); err == nil {
		t.Fatal("enabled without the dialog")
	}
	st, _ := e.s.Status()
	if st.Enabled || st.Running {
		t.Fatalf("status = %+v", st)
	}
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(st.Port), time.Second); err == nil {
		c.Close()
		t.Fatal("a listener started")
	}
	if _, err := e.s.CreateToken(t.Context(), "claude"); err == nil || e.copied != "" {
		t.Fatal("a token was created without the dialog")
	}
}

// The whole path: an approved command runs on the host's lease without a
// terminal, a denied one runs nothing, and no result, binding or token
// file holds the host's password or the token.
func TestMCPEndToEnd(t *testing.T) {
	e := newMCPEnv(t)
	if err := e.s.SetEnabled(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	nt, err := e.s.CreateToken(t.Context(), "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(nt)
	if e.copied == "" || strings.Contains(string(b), e.copied) || !strings.Contains(nt.Config, "HOSTBAY_MCP_TOKEN") || strings.Contains(nt.Config, e.copied) {
		t.Fatalf("new token result = %s", b)
	}
	list, _ := e.s.ListTokens()
	if lb, _ := json.Marshal(list); strings.Contains(string(lb), e.copied) {
		t.Fatal("ListTokens returned the token")
	}
	cs := e.client(t, e.copied)

	var responses []string
	_, text := mcpCall(t, cs, "list_connections", nil)
	responses = append(responses, text)
	if !strings.Contains(text, e.host) {
		t.Fatalf("list_connections = %s", text)
	}

	done := make(chan string, 1)
	go func() {
		_, text := mcpCall(t, cs, agentexec.ToolRun, map[string]any{"hostId": e.host, "command": "echo ok; echo " + mcpSamplePassword})
		done <- text
	}()
	p := e.answer(t, true)
	if p.Client != "claude-code" || p.Host != "web" {
		t.Fatalf("card = %+v", p)
	}
	text = <-done
	responses = append(responses, text)
	if !strings.Contains(text, `"stdout":"ok\n[secret removed]\n"`) || !strings.Contains(text, `"exitCode":0`) {
		t.Fatalf("run_command = %s", text)
	}
	if got := e.srv.Commands(); len(got) != 1 || got[0] != "echo ok; echo "+mcpSamplePassword {
		t.Fatalf("server ran %q", got)
	}

	go func() {
		_, text := mcpCall(t, cs, agentexec.ToolRun, map[string]any{"hostId": e.host, "command": "touch /tmp/denied"})
		done <- text
	}()
	e.answer(t, false)
	text = <-done
	responses = append(responses, text)
	if !strings.Contains(text, `"denied"`) || len(e.srv.Commands()) != 1 {
		t.Fatalf("denied = %s, server ran %q", text, e.srv.Commands())
	}

	for _, r := range responses {
		if strings.Contains(r, mcpSamplePassword) || strings.Contains(r, e.copied) {
			t.Fatalf("a response holds a secret: %s", r)
		}
	}
	// The secret scan: no Hostbay file, in the data folder or the default
	// folder, and no plain backup holds the token or the host's password,
	// readable or in obf: form.
	obf := func(s string) string {
		return "obf:" + base64.StdEncoding.EncodeToString([]byte(backup.Deobfuscate("obf:"+base64.StdEncoding.EncodeToString([]byte(s)))))
	}
	samples := []string{e.copied, e.copied[6:], obf(e.copied), mcpSamplePassword, obf(mcpSamplePassword)}
	scan := func(where string, b []byte) {
		for _, s := range samples {
			if strings.Contains(string(b), s) {
				t.Errorf("%s holds %q", where, s)
			}
		}
	}
	scanned := 0
	for _, dir := range []string{e.s.st.Dir(), e.s.st.Home()} {
		filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				scanned++
				b, _ := os.ReadFile(p)
				scan(p, b)
			}
			return nil
		})
	}
	d, _ := e.s.st.Data()
	plain, err := backup.Export(d, e.conns.creds.vault, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scan("the plain backup", plain)
	if scanned < 3 {
		t.Fatalf("scanned %d files", scanned)
	}
}

// A host whose key is not trusted fails back as HostKeyUnknown with no
// trust prompt and no credential sent; one with no saved password fails as
// SecretNeeded.
func TestMCPNoPrompts(t *testing.T) {
	e := newMCPEnv(t)
	other := sftptest.New(t, "other-pw") // its key is in no known_hosts
	h, p, _ := net.SplitHostPort(other.Addr)
	port, _ := strconv.Atoi(p)
	pw, auth := "other-pw", "password"
	unknown, _ := e.hosts.SaveHost(Host{Alias: "new", Hostname: h, Port: port, User: "u", AuthType: &auth, Password: &pw})
	h, p, _ = net.SplitHostPort(e.srv.Addr)
	port, _ = strconv.Atoi(p)
	nopw, _ := e.hosts.SaveHost(Host{Alias: "nopw", Hostname: h, Port: port, User: "u", AuthType: &auth})

	e.s.SetEnabled(t.Context(), true)
	nt, _ := e.s.CreateToken(t.Context(), "c")
	tok := e.copied
	for host, want := range map[string]string{unknown.ID: "HostKeyUnknown", nopw.ID: "SecretNeeded"} {
		if err := e.s.AddAllowEntry(t.Context(), nt.Token.ID, host, "id"); err != nil {
			t.Fatal(err)
		}
		cs := e.client(t, tok)
		_, text := mcpCall(t, cs, agentexec.ToolRun, map[string]any{"hostId": host, "command": "id"})
		if !strings.Contains(text, want) || strings.Contains(text, "requestId") {
			t.Fatalf("%s = %s", want, text)
		}
	}
	e.conns.trust.mu.Lock()
	n := len(e.conns.trust.pending)
	e.conns.trust.mu.Unlock()
	if n != 0 || len(other.Commands()) != 0 || len(e.srv.Commands()) != 0 {
		t.Fatalf("trust prompts %d, commands %q %q", n, other.Commands(), e.srv.Commands())
	}
}

// Revoking a token denies its waiting request and new calls get 401.
func TestMCPRevoke(t *testing.T) {
	e := newMCPEnv(t)
	e.s.SetEnabled(t.Context(), true)
	nt, _ := e.s.CreateToken(t.Context(), "c")
	cs := e.client(t, e.copied)
	done := make(chan string, 1)
	go func() {
		_, text := mcpCall(t, cs, agentexec.ToolRun, map[string]any{"hostId": e.host, "command": "id"})
		done <- text
	}()
	for len(e.s.srv.Jobs.Approvals.List()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := e.s.RevokeToken(nt.Token.ID); err != nil {
		t.Fatal(err)
	}
	if text := <-done; !strings.Contains(text, `"denied"`) {
		t.Fatalf("after revoke = %s", text)
	}
	st, _ := e.s.Status()
	if _, err := mcptest.HTTP(t.Context(), st.Endpoint, e.copied, mcptest.V20260728); err == nil || !strings.Contains(err.Error(), "401") && !strings.Contains(strings.ToLower(err.Error()), "unauthorized") {
		t.Fatalf("revoked token connected: %v", err)
	}
	if len(e.srv.Commands()) != 0 {
		t.Fatalf("server ran %q", e.srv.Commands())
	}
}

// The MCP folder cannot be the data folder, ~/.ssh or a parent of either.
func TestMCPPickFolderRefuses(t *testing.T) {
	e := newMCPEnv(t)
	ssh := filepath.Join(t.TempDir(), ".ssh")
	os.MkdirAll(ssh, 0o700)
	e.s.sshDir = func() (string, error) { return ssh, nil }
	for _, dir := range []string{e.s.st.Dir(), filepath.Dir(e.s.st.Dir()), ssh, filepath.Dir(ssh)} {
		e.s.pick = func(string) (string, error) { return dir, nil }
		if _, err := e.s.PickFolder(); err == nil {
			t.Errorf("%s accepted", dir)
		}
	}
	ok := t.TempDir()
	e.s.pick = func(string) (string, error) { return ok, nil }
	if got, err := e.s.PickFolder(); err != nil || got != ok {
		t.Fatalf("good folder: %q %v", got, err)
	}
	if l, _ := e.s.st.Local(); l.MCP.Folder != ok {
		t.Fatalf("saved folder %q", l.MCP.Folder)
	}
	// The page's own setter cannot change MCP settings.
	if err := NewSettingsService(e.s.st).SetLocal(map[string]any{"mcp": map[string]any{"enabled": true}}); err == nil {
		t.Fatal("SetLocal changed MCP settings")
	}
	_ = store.MCPSettings{}
}

// VPN allow entries are added only through the dialog, never for a
// system-wide profile, go stale when the profile's gateway changes, and
// leave with their token (KTD6).
func TestMCPVPNAllowSettings(t *testing.T) {
	e := newMCPEnv(t)
	nt, _ := e.s.CreateToken(t.Context(), "c")
	vpns := e.s.srv.VPNs.(*mcpVPNs).creds.vpns
	app, err := vpns.SaveVPN(VPN{Name: "office", Type: "wireguard", Tier: "app", Gateway: "a.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	whole, _ := vpns.SaveVPN(VPN{Name: "whole", Type: "openvpn", Tier: "system", Gateway: "w.example.test"})
	ext, _ := vpns.SaveVPN(VPN{Name: "forti", Type: "other", Tier: "external", Gateway: "f.example.test"})

	dialogs := e.dialogs
	if err := e.s.AddVPNAllow(t.Context(), nt.Token.ID, whole.ID); err == nil || e.dialogs != dialogs {
		t.Fatalf("system-wide entry: %v, %d dialogs", err, e.dialogs-dialogs)
	}
	if err := e.s.AddVPNAllow(t.Context(), nt.Token.ID, ext.ID); err == nil || e.dialogs != dialogs {
		t.Fatalf("external entry: %v, %d dialogs", err, e.dialogs-dialogs)
	}
	e.confirm = false
	if err := e.s.AddVPNAllow(t.Context(), nt.Token.ID, app.ID); err == nil {
		t.Fatal("added without the dialog")
	}
	e.confirm = true
	if err := e.s.AddVPNAllow(t.Context(), nt.Token.ID, app.ID); err != nil {
		t.Fatal(err)
	}
	l, _ := e.s.ListVPNAllow()
	if len(l.Entries) != 1 || l.Entries[0].VPNName != "office" || l.Entries[0].TokenName != "c" || l.Entries[0].Stale ||
		len(l.Profiles) != 1 || l.Profiles[0].ID != app.ID {
		t.Fatalf("list = %+v", l)
	}

	app.Gateway = "elsewhere.example.test"
	vpns.SaveVPN(app)
	if l, _ := e.s.ListVPNAllow(); len(l.Entries) != 1 || !l.Entries[0].Stale {
		t.Fatalf("after a gateway change = %+v", l)
	}

	if err := e.s.RevokeToken(nt.Token.ID); err != nil {
		t.Fatal(err)
	}
	if loc, _ := e.s.st.Local(); len(loc.MCP.VPNAllow) != 0 {
		t.Fatalf("entries outlived their token: %+v", loc.MCP.VPNAllow)
	}
}

// F4 against a test profile: the agent connects by tag after the card, the
// profile is its own, and it disconnects it after another card.
func TestMCPConnectVPNEndToEnd(t *testing.T) {
	e := newMCPEnv(t)
	e.s.SetEnabled(t.Context(), true)
	e.s.CreateToken(t.Context(), "claude-code")
	vpns := e.s.srv.VPNs.(*mcpVPNs).creds.vpns
	fake := vpntest.Install(t, vpntest.OK)
	vpns.helper = func() (string, error) { return fake.Path, nil }
	vpns.emit = func(string, any) {}
	t.Cleanup(func() { vpns.ServiceShutdown() })
	key := testWGKey
	v, err := vpns.SaveVPN(VPN{Name: "Atlas VPN", Type: "wireguard", Tier: "app", Gateway: "vpn.example.test", Address: []string{"10.9.0.2/32"},
		PeerKey: testPeer, Routes: []string{"10.9.0.0/24"}, Tags: []string{"atlas"}, PrivateKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	cs := e.client(t, e.copied)

	done := make(chan string, 1)
	go func() { _, text := mcpCall(t, cs, "connect_vpn", map[string]any{"tag": "atlas"}); done <- text }()
	if p := e.answer(t, true); p.Client != "claude-code" || p.Host != "Atlas VPN" || p.Tag != "atlas" || p.Gateway != "vpn.example.test" {
		t.Fatalf("card = %+v", p)
	}
	if text := <-done; !strings.Contains(text, `"state":"connected"`) || !strings.Contains(text, `"connectedBy":"you"`) || strings.Contains(text, strconv.Itoa(vpntest.Port)) {
		t.Fatalf("connect_vpn = %s", text)
	}

	go func() { _, text := mcpCall(t, cs, "disconnect_vpn", map[string]any{"profileId": v.ID}); done <- text }()
	e.answer(t, true)
	if text := <-done; !strings.Contains(text, `"state":"disconnected"`) || vpns.ConnectedVPNs() != 0 {
		t.Fatalf("disconnect_vpn = %s", text)
	}
}
