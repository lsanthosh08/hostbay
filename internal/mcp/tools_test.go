package mcp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"hostbay/internal/agentexec"
	"hostbay/internal/apperr"
	"hostbay/internal/mcp/mcptest"
	"hostbay/internal/store"
)

// A client with a valid token lists the saved SSH hosts with nothing
// sealed and no key path, and an approved `echo ok` returns ok with exit
// code 0, on both protocol versions in stateless mode.
func TestToolsListAndRun(t *testing.T) {
	for _, v := range []string{mcptest.V20251125, mcptest.V20260728} {
		t.Run(v, func(t *testing.T) {
			r := newRig(t)
			cs := r.session(t, v)
			if got := cs.InitializeResult().ProtocolVersion; got != v {
				t.Fatalf("negotiated %q", got)
			}
			res, text := call(t, cs, "list_connections", nil)
			noSecrets(t, "list_connections", text)
			var list connections
			if err := json.Unmarshal([]byte(text), &list); err != nil || res.IsError {
				t.Fatalf("list = %s (%v)", text, err)
			}
			if len(list.Connections) != 2 || list.Connections[0].ID != "h1" || list.Connections[0].Hostname != "10.0.0.5" {
				t.Fatalf("connections = %+v", list.Connections)
			}
			if strings.Contains(text, "sealed") || strings.Contains(text, "t1") {
				t.Fatalf("list holds a sealed field or a non-SSH host: %s", text)
			}

			done := make(chan string, 1)
			go func() {
				_, text := call(t, cs, agentexec.ToolRun, map[string]any{"hostId": "h1", "command": "echo ok"})
				done <- text
			}()
			p := r.approveNext(t, true)
			if p.Client != "claude-code" || p.Host != "web-h1" || p.Bytes != "echo ok" {
				t.Fatalf("card = %+v", p)
			}
			text = <-done
			noSecrets(t, "run_command", text)
			var job agentexec.HostJobView
			json.Unmarshal([]byte(text), &job)
			if job.State != agentexec.JobDone || job.Exit == nil || *job.Exit != 0 || job.Stdout != "ok\n" {
				t.Fatalf("job = %s", text)
			}
		})
	}
}

// A denied request is refused and nothing runs.
func TestToolsDenied(t *testing.T) {
	r := newRig(t)
	cs := r.session(t, mcptest.V20260728)
	done := make(chan string, 1)
	go func() {
		_, text := call(t, cs, agentexec.ToolRun, map[string]any{"hostId": "h1", "command": "rm -rf /tmp/x"})
		done <- text
	}()
	r.approveNext(t, false)
	if text := <-done; !strings.Contains(text, `"denied"`) {
		t.Fatalf("denied = %s", text)
	}
	if len(r.hosts.runs()) != 0 {
		t.Fatalf("ran %q", r.hosts.runs())
	}
}

// An unanswered run_command returns within the wait with its job ID and
// awaiting_approval; approved later, job_status reports done.
func TestToolsApprovalTimeout(t *testing.T) {
	r := newRig(t)
	r.srv.Jobs.Wait = 100 * time.Millisecond
	cs := r.session(t, mcptest.V20251125)
	_, text := call(t, cs, agentexec.ToolRun, map[string]any{"hostId": "h1", "command": "echo later"})
	var job agentexec.HostJobView
	json.Unmarshal([]byte(text), &job)
	if job.State != agentexec.JobAwaiting || job.ID == "" {
		t.Fatalf("first result = %s", text)
	}
	r.approveNext(t, true)
	for range 200 {
		_, text = call(t, cs, "job_status", map[string]any{"jobId": job.ID})
		if strings.Contains(text, `"done"`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(text, `"stdout":"later\n"`) {
		t.Fatalf("job_status = %s", text)
	}
	noSecrets(t, "job_status", text)
}

// Another token cannot read a job: it is not found.
func TestToolsJobBoundToToken(t *testing.T) {
	r := newRig(t)
	e, _ := agentexec.ParseEntry("echo mine", false)
	r.allow(t, r.tokID, "h1", e)
	cs := r.session(t, mcptest.V20260728)
	_, text := call(t, cs, agentexec.ToolRun, map[string]any{"hostId": "h1", "command": "echo mine"})
	var job agentexec.HostJobView
	json.Unmarshal([]byte(text), &job)

	_, other, _ := r.srv.Tokens.Create("other")
	b, err := mcptest.HTTP(t.Context(), r.url, other, mcptest.V20260728)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	res, text := call(t, b, "job_status", map[string]any{"jobId": job.ID})
	if !res.IsError || !strings.Contains(text, "not found") {
		t.Fatalf("token B read token A's job: %s", text)
	}
}

func (r *rig) allow(t *testing.T, tokenID, host string, e store.AllowEntry) {
	t.Helper()
	err := r.st.EditLocal(func(l *store.Local) error {
		l.MCP.Allowlist = map[string]map[string][]store.AllowEntry{tokenID: {host: {e}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An exact entry for this token and host runs without a card; the same
// command from another token asks, and so does one with more after it.
func TestToolsAllowlist(t *testing.T) {
	r := newRig(t)
	e, _ := agentexec.ParseEntry("systemctl status nginx", false)
	r.allow(t, r.tokID, "h1", e)
	cs := r.session(t, mcptest.V20260728)
	if _, text := call(t, cs, agentexec.ToolRun, map[string]any{"hostId": "h1", "command": "systemctl status nginx"}); !strings.Contains(text, `"done"`) {
		t.Fatalf("allowlisted = %s", text)
	}
	done := make(chan struct{})
	go func() {
		call(t, cs, agentexec.ToolRun, map[string]any{"hostId": "h1", "command": "systemctl status nginx; id"})
		close(done)
	}()
	if p := r.approveNext(t, false); p.Bytes != "systemctl status nginx; id" {
		t.Fatalf("card = %+v", p)
	}
	<-done

	_, other, _ := r.srv.Tokens.Create("other")
	b, err := mcptest.HTTP(t.Context(), r.url, other, mcptest.V20260728)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	done = make(chan struct{})
	go func() {
		call(t, b, agentexec.ToolRun, map[string]any{"hostId": "h1", "command": "systemctl status nginx"})
		close(done)
	}()
	r.approveNext(t, false)
	<-done
	if got := r.hosts.runs(); len(got) != 1 {
		t.Fatalf("ran %q", got)
	}
}

// No prompt reaches a client: a host Hostbay cannot dial without one fails
// with its code, and a request naming anything but a saved SSH host, or an
// unsaved host and port, is refused before a card.
func TestToolsNoPromptsAndRefusals(t *testing.T) {
	r := newRig(t)
	r.hosts.fail["h1"] = apperr.New(apperr.HostKeyUnknown, "The authenticity of 10.0.0.5 port 22 cannot be established")
	r.hosts.fail["h2"] = apperr.New(apperr.SecretNeeded, "No password is saved for this host; enter it.")
	for _, h := range []string{"h1", "h2"} {
		e, _ := agentexec.ParseEntry("id", false)
		r.allow(t, r.tokID, h, e)
		cs := r.session(t, mcptest.V20260728)
		_, text := call(t, cs, agentexec.ToolRun, map[string]any{"hostId": h, "command": "id"})
		want := map[string]string{"h1": "HostKeyUnknown", "h2": "SecretNeeded"}[h]
		if !strings.Contains(text, `"failed"`) || !strings.Contains(text, want) {
			t.Fatalf("%s = %s", h, text)
		}
	}
	cs := r.session(t, mcptest.V20260728)
	for _, h := range []string{"t1", "local", "10.0.0.9:22"} {
		res, text := call(t, cs, agentexec.ToolRun, map[string]any{"hostId": h, "command": "id"})
		if !res.IsError {
			t.Fatalf("%s accepted: %s", h, text)
		}
	}
	if len(r.srv.Jobs.Approvals.List()) != 0 {
		t.Fatal("a refused request showed a card")
	}
}

// A client launched over stdio must put a file in the MCP folder before an
// upload and read it there after a download, so it has to learn the path.
func TestListConnectionsNamesTheMCPFolder(t *testing.T) {
	r := newRig(t)
	cs := r.session(t, mcptest.V20260728)
	var list connections
	_, text := call(t, cs, "list_connections", nil)
	if err := json.Unmarshal([]byte(text), &list); err != nil || list.MCPFolder != "" {
		t.Fatalf("with no folder set: %s (%v)", text, err)
	}
	dir := t.TempDir()
	r.st.EditLocal(func(l *store.Local) error { l.MCP.Folder = dir; return nil })
	_, text = call(t, cs, "list_connections", nil)
	if err := json.Unmarshal([]byte(text), &list); err != nil || list.MCPFolder != dir {
		t.Fatalf("the client cannot find the MCP folder: %s (%v)", text, err)
	}
}

// Transfers stay inside the MCP folder: no way out by .. or a symlink,
// and a download never replaces a file.
func TestToolsTransfersStayInFolder(t *testing.T) {
	r := newRig(t)
	dir := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "id_rsa"), []byte(samplePassKey), 0o600)
	if err := os.Symlink(outside, filepath.Join(dir, "ssh")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	os.WriteFile(filepath.Join(dir, "have.txt"), []byte("x"), 0o600)
	r.st.EditLocal(func(l *store.Local) error { l.MCP.Folder = dir; return nil })
	cs := r.session(t, mcptest.V20260728)

	for _, args := range []map[string]any{
		{"hostId": "h1", "remotePath": "/tmp/x", "localPath": "../x"},
		{"hostId": "h1", "remotePath": "/tmp/x", "localPath": "have.txt"},
		{"hostId": "h1", "remotePath": "/tmp/x", "localPath": filepath.Join(outside, "y")},
	} {
		if res, text := call(t, cs, agentexec.ToolDownload, args); !res.IsError {
			t.Fatalf("download %v accepted: %s", args, text)
		}
	}
	// Through the symlink: approved, the open is refused by os.Root.
	done := make(chan string, 1)
	go func() {
		_, text := call(t, cs, agentexec.ToolUpload, map[string]any{"hostId": "h1", "remotePath": "/tmp/k", "localPath": "ssh/id_rsa"})
		done <- text
	}()
	r.approveNext(t, true)
	if text := <-done; !strings.Contains(text, `"failed"`) {
		t.Fatalf("upload through symlink = %s", text)
	}
	if _, err := os.Stat(filepath.Join(outside, "y")); err == nil {
		t.Fatal("a file was written outside the MCP folder")
	}

	go func() {
		_, text := call(t, cs, agentexec.ToolDownload, map[string]any{"hostId": "h1", "remotePath": "/tmp/x", "localPath": "new.txt"})
		done <- text
	}()
	r.approveNext(t, true)
	if text := <-done; !strings.Contains(text, `"done"`) {
		t.Fatalf("download = %s", text)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "new.txt")); string(b) != "remote bytes" {
		t.Fatalf("downloaded %q", b)
	}
}

// A client that goes away while its request waits withdraws the card, over
// HTTP and through the relay, and nothing runs.
func TestToolsClientGoneWithdrawsCard(t *testing.T) {
	r := newRig(t)
	waitEmpty := func(how string) {
		t.Helper()
		for range 200 {
			if len(r.srv.Jobs.Approvals.List()) == 0 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("%s: the card stayed: %v", how, r.srv.Jobs.Approvals.List())
	}
	waitCard := func() {
		t.Helper()
		for len(r.srv.Jobs.Approvals.List()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}

	cs := r.session(t, mcptest.V20260728)
	ctx, cancel := context.WithCancel(t.Context())
	go cs.CallTool(ctx, &sdk.CallToolParams{Name: agentexec.ToolRun, Arguments: map[string]any{"hostId": "h1", "command": "echo gone"}})
	waitCard()
	cancel()
	waitEmpty("http")

	run := startRelay(r.server, r.token)
	io.WriteString(run.inW, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"run_command","arguments":{"hostId":"h2","command":"echo gone"}}}`+"\n")
	waitCard()
	run.inW.Close() // the client exits
	waitEmpty("relay")
	if got := r.hosts.runs(); len(got) != 0 {
		t.Fatalf("ran %q", got)
	}
}
