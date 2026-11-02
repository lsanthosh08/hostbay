package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/agentexec"
	"hostbay/internal/container"
	"hostbay/internal/mcp"
	"hostbay/internal/sftp"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
)

// MCPService is Settings > MCP and the server behind it (R63, KTD35): off
// by default, and every change that widens trust (turning it on, creating
// a token, an allowlist entry) goes through a dialog Go opens and OS
// verification. A token is never sent to the page; Go copies it to the
// clipboard once. Everything here is device-local and never syncs.
type MCPService struct {
	st      *store.Store
	srv     *mcp.Server
	confirm container.Confirm
	gate    func(ctx context.Context, reason string) error
	copy    func(text string) bool             // the system clipboard, written by Go
	pick    func(title string) (string, error) // a folder dialog Go opens
	sshDir  func() (string, error)             // ~/.ssh
	Raise   func()                             // shows the main window when an MCP request waits; set by main
	mu      sync.Mutex
	lastErr string
	waiting int // MCP requests in the tray, to raise the window on a new one
}

// NewMCPService builds the server over the agent's executor's tray, so
// MCP requests show in the same approval tray (KTD32). The endpoint file
// goes in the default folder, beside the lock, so it stays put when the
// data folder moves (KTD42).
func NewMCPService(st *store.Store, conns *ConnService, creds *HostCredentials, ai *AIService) *MCPService {
	jobs := agentexec.NewHostJobs(ai.exec.Approvals, &mcpHosts{conns: conns, creds: creds})
	s := &MCPService{
		st:      st,
		srv:     &mcp.Server{St: st, Jobs: jobs, Tokens: mcp.NewTokens(st), VPNs: &mcpVPNs{creds: creds}, Home: st.Home()},
		confirm: container.NativeConfirm,
		gate:    ai.gate,
		copy:    func(text string) bool { return application.Get().Clipboard.SetText(text) },
		pick:    PickDataFolder,
		sshDir: func() (string, error) {
			h, err := os.UserHomeDir()
			return filepath.Join(h, ".ssh"), err
		},
	}
	prev := jobs.Approvals.OnChange
	jobs.Approvals.OnChange = func(l []agentexec.Pending) {
		if prev != nil {
			prev(l)
		}
		s.raise(l)
	}
	return s
}

// raise shows the window when a new MCP request joins the tray, since a
// client's user may be in another app.
func (s *MCPService) raise(l []agentexec.Pending) {
	n := 0
	for _, p := range l {
		if strings.HasPrefix(p.Principal, "mcp:") {
			n++
		}
	}
	s.mu.Lock()
	more := n > s.waiting
	s.waiting = n
	s.mu.Unlock()
	if more && s.Raise != nil {
		s.Raise()
	}
}

// ServiceStartup starts the server when it was left on, and clears an
// endpoint file a crashed run left behind.
func (s *MCPService) ServiceStartup(context.Context, application.ServiceOptions) error {
	mcp.ClearEndpoint(s.srv.Home)
	if l, err := s.st.Local(); err == nil && l.MCP.Enabled {
		s.start(l.MCP)
	}
	return nil
}

// ServiceShutdown stops the server and removes its endpoint file.
func (s *MCPService) ServiceShutdown() error {
	s.srv.Stop()
	return nil
}

func port(m store.MCPSettings) int {
	if m.Port == 0 {
		return mcp.DefaultPort
	}
	return m.Port
}

func (s *MCPService) start(m store.MCPSettings) {
	err := s.srv.Start(port(m))
	s.mu.Lock()
	s.lastErr = ""
	if err != nil {
		s.lastErr = err.Error()
	}
	s.mu.Unlock()
}

// MCPStatus is Settings > MCP's header.
type MCPStatus struct {
	Enabled  bool   `json:"enabled"`
	Running  bool   `json:"running"`
	Port     int    `json:"port"`
	Endpoint string `json:"endpoint"`
	Error    string `json:"error,omitempty"` // why an enabled server is not running
	Folder   string `json:"folder"`
}

func (s *MCPService) Status() (MCPStatus, error) {
	l, err := s.st.Local()
	if err != nil {
		return MCPStatus{}, err
	}
	p := port(l.MCP)
	s.mu.Lock()
	defer s.mu.Unlock()
	return MCPStatus{Enabled: l.MCP.Enabled, Running: s.srv.Port() != 0, Port: p, Endpoint: "http://127.0.0.1:" + strconv.Itoa(p) + mcp.Path,
		Error: s.lastErr, Folder: l.MCP.Folder}, nil
}

// widen asks in a dialog Go opens, then through OS verification (KTD32).
func (s *MCPService) widen(ctx context.Context, title, message string) error {
	return widen(ctx, s.confirm, s.gate, title, message)
}

// SetEnabled turns the server on, after the dialog and verification, or off.
func (s *MCPService) SetEnabled(ctx context.Context, on bool) error {
	if on {
		if err := s.widen(ctx, "Turn on Hostbay's MCP server?",
			"AI clients on this computer that hold one of your MCP tokens can ask to run commands and copy files on your saved SSH hosts. Each request waits for your approval in Hostbay."); err != nil {
			return err
		}
	}
	var m store.MCPSettings
	err := s.st.EditLocal(func(l *store.Local) error {
		l.MCP.Enabled = on
		m = l.MCP
		return nil
	})
	if err != nil {
		return err
	}
	s.srv.Stop()
	if on {
		s.start(m)
	}
	return nil
}

// SetPort changes the port, restarting a running server on it.
func (s *MCPService) SetPort(p int) error {
	if p < 1024 || p > 65535 {
		return errors.New("choose a port from 1024 to 65535")
	}
	var m store.MCPSettings
	err := s.st.EditLocal(func(l *store.Local) error {
		l.MCP.Port = p
		m = l.MCP
		return nil
	})
	if err != nil {
		return err
	}
	if m.Enabled {
		s.srv.Stop()
		s.start(m)
	}
	return nil
}

// MCPToken is a token as the page sees it: never the token itself.
type MCPToken struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Created  int64  `json:"created"`
	LastUsed int64  `json:"lastUsed,omitempty"`
}

func (s *MCPService) ListTokens() ([]MCPToken, error) {
	all, err := s.srv.Tokens.List()
	if err != nil {
		return nil, err
	}
	out := make([]MCPToken, len(all))
	for i, t := range all {
		out[i] = MCPToken{ID: t.ID, Name: t.Name, Created: t.Created, LastUsed: t.LastUsed}
	}
	return out, nil
}

// MCPNewToken is a token just created: Go put the token on the clipboard,
// and Config is a client config that names HOSTBAY_MCP_TOKEN instead.
type MCPNewToken struct {
	Token  MCPToken `json:"token"`
	Copied bool     `json:"copied"`
	Config string   `json:"config"`
}

// CreateToken makes a token for one client, after the dialog and
// verification. Only its hash is kept, so this is the one time it exists
// outside the client's config.
func (s *MCPService) CreateToken(ctx context.Context, name string) (MCPNewToken, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return MCPNewToken{}, errors.New("name the client this token is for")
	}
	if err := s.widen(ctx, "Create an MCP token for "+name+"?",
		"The client that holds it can ask to run commands and copy files on your saved SSH hosts, each request approved in Hostbay. Hostbay copies the token to the clipboard once; paste it into the client's HOSTBAY_MCP_TOKEN."); err != nil {
		return MCPNewToken{}, err
	}
	tok, secret, err := s.srv.Tokens.Create(name)
	if err != nil {
		return MCPNewToken{}, err
	}
	st, _ := s.Status()
	exe, _ := os.Executable()
	cfg := fmt.Sprintf(`{
  "mcpServers": {
    "hostbay": {
      "type": "http",
      "url": %q,
      "headers": { "Authorization": "Bearer ${HOSTBAY_MCP_TOKEN}" }
    },
    "hostbay-stdio": {
      "command": %q,
      "args": ["mcp"],
      "env": { "HOSTBAY_MCP_TOKEN": "${HOSTBAY_MCP_TOKEN}" }
    }
  }
}`, st.Endpoint, exe)
	return MCPNewToken{Token: MCPToken{ID: tok.ID, Name: tok.Name, Created: tok.Created}, Copied: s.copy(secret), Config: cfg}, nil
}

// RevokeToken deletes a token and its allowlist and VPN entries: new calls get
// 401, its waiting requests are denied, and its running jobs finish with
// their output dropped (KTD36).
func (s *MCPService) RevokeToken(id string) error {
	if _, err := s.srv.Tokens.Revoke(id); err != nil {
		return err
	}
	s.srv.Jobs.Revoke("mcp:" + id)
	return s.st.EditLocal(func(l *store.Local) error {
		delete(l.MCP.Allowlist, id)
		l.MCP.VPNAllow = slices.DeleteFunc(l.MCP.VPNAllow, func(e store.VPNAllowEntry) bool { return e.Token == id })
		return nil
	})
}

// MCPAllowEntry is one command a token may run on a host without a card.
type MCPAllowEntry struct {
	TokenID   string `json:"tokenId"`
	TokenName string `json:"tokenName"`
	HostID    string `json:"hostId"`
	HostLabel string `json:"hostLabel"`
	Command   string `json:"command"`
}

func (s *MCPService) ListAllowlist() ([]MCPAllowEntry, error) {
	l, err := s.st.Local()
	if err != nil {
		return nil, err
	}
	d, err := s.st.Data()
	if err != nil {
		return nil, err
	}
	tokens, _ := s.srv.Tokens.List()
	name := func(id string) string {
		if i := slices.IndexFunc(tokens, func(t mcp.Token) bool { return t.ID == id }); i >= 0 {
			return tokens[i].Name
		}
		return "Revoked token"
	}
	out := []MCPAllowEntry{}
	for tok, hosts := range l.MCP.Allowlist {
		for host, es := range hosts {
			for _, e := range es {
				out = append(out, MCPAllowEntry{TokenID: tok, TokenName: name(tok), HostID: host, HostLabel: hostLabel(d, host), Command: agentexec.Command(e)})
			}
		}
	}
	slices.SortFunc(out, func(a, b MCPAllowEntry) int {
		return strings.Compare(a.TokenName+"\x00"+a.HostLabel+"\x00"+a.Command, b.TokenName+"\x00"+b.HostLabel+"\x00"+b.Command)
	})
	return out, nil
}

// AddAllowEntry lets one token run exactly command on one saved SSH host
// without a card, after the dialog and verification. MCP entries are
// exact; there are no prefix entries, modes or bypass (KTD36).
func (s *MCPService) AddAllowEntry(ctx context.Context, tokenID, hostID, command string) error {
	e, err := agentexec.ParseEntry(command, false)
	if err != nil {
		return err
	}
	tokens, err := s.srv.Tokens.List()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(tokens, func(t mcp.Token) bool { return t.ID == tokenID })
	if i < 0 {
		return errors.New("token not found")
	}
	d, err := s.st.Data()
	if err != nil {
		return err
	}
	if h := d.Host(hostID); h == nil || h.KindName() != "ssh" {
		return errors.New("choose a saved SSH host")
	}
	if err := s.widen(ctx, "Let "+tokens[i].Name+" run this without asking?",
		fmt.Sprintf("On %s, the MCP client %s will run exactly %q without asking. The command runs as the remote shell sees it, aliases included.", hostLabel(d, hostID), tokens[i].Name, agentexec.Command(e))); err != nil {
		return err
	}
	return s.st.EditLocal(func(l *store.Local) error {
		if l.MCP.Allowlist == nil {
			l.MCP.Allowlist = map[string]map[string][]store.AllowEntry{}
		}
		if l.MCP.Allowlist[tokenID] == nil {
			l.MCP.Allowlist[tokenID] = map[string][]store.AllowEntry{}
		}
		for _, x := range l.MCP.Allowlist[tokenID][hostID] {
			if slices.Equal(x.Argv, e.Argv) {
				return nil
			}
		}
		l.MCP.Allowlist[tokenID][hostID] = append(l.MCP.Allowlist[tokenID][hostID], e)
		return nil
	})
}

// RemoveAllowEntry takes an entry off; that only makes more ask.
func (s *MCPService) RemoveAllowEntry(tokenID, hostID, command string) error {
	return s.st.EditLocal(func(l *store.Local) error {
		hosts := l.MCP.Allowlist[tokenID]
		if hosts == nil {
			return nil
		}
		hosts[hostID] = slices.DeleteFunc(hosts[hostID], func(x store.AllowEntry) bool { return agentexec.Command(x) == command })
		if len(hosts[hostID]) == 0 {
			delete(hosts, hostID)
		}
		if len(hosts) == 0 {
			delete(l.MCP.Allowlist, tokenID)
		}
		return nil
	})
}

// MCPVPNAllow is Settings > MCP's VPN entries, and the profiles one can
// name: app-scoped ones, since a system-wide profile always asks (KTD6) and
// an external one connects in its own app, so a card never gates it.
type MCPVPNAllow struct {
	Entries  []MCPVPNAllowEntry `json:"entries"`
	Profiles []MCPVPNChoice     `json:"profiles"`
}

// MCPVPNAllowEntry is one profile a token connects and disconnects without
// a card. Stale says the profile's type, gateway or tier changed since, so
// it asks again until the entry is added anew.
type MCPVPNAllowEntry struct {
	TokenID   string `json:"tokenId"`
	TokenName string `json:"tokenName"`
	VPNID     string `json:"vpnId"`
	VPNName   string `json:"vpnName"`
	Stale     bool   `json:"stale,omitempty"`
}

type MCPVPNChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListVPNAllow lists the VPN entries; one whose token or profile is gone
// applies to nothing and is left out.
func (s *MCPService) ListVPNAllow() (MCPVPNAllow, error) {
	l, err := s.st.Local()
	if err != nil {
		return MCPVPNAllow{}, err
	}
	d, err := s.st.Data()
	if err != nil {
		return MCPVPNAllow{}, err
	}
	tokens, err := s.srv.Tokens.List()
	if err != nil {
		return MCPVPNAllow{}, err
	}
	out := MCPVPNAllow{Entries: []MCPVPNAllowEntry{}, Profiles: []MCPVPNChoice{}}
	for _, e := range l.MCP.VPNAllow {
		i := slices.IndexFunc(tokens, func(t mcp.Token) bool { return t.ID == e.Token })
		v := d.VPN(e.Profile)
		if i < 0 || v == nil {
			continue
		}
		out.Entries = append(out.Entries, MCPVPNAllowEntry{TokenID: e.Token, TokenName: tokens[i].Name, VPNID: v.ID, VPNName: v.Name, Stale: !e.Matches(e.Token, v)})
	}
	for _, v := range d.VPNs.Items {
		if v.Tier != store.VPNTierSystem && v.Tier != store.VPNTierExternal {
			out.Profiles = append(out.Profiles, MCPVPNChoice{ID: v.ID, Name: v.Name})
		}
	}
	return out, nil
}

// AddVPNAllow lets one token connect and disconnect one app-scoped VPN profile without a card, after the dialog and verification.
// The entry holds while the profile keeps its type, gateway and tier; a
// system-wide profile is refused, since it always asks (KTD6).
func (s *MCPService) AddVPNAllow(ctx context.Context, tokenID, vpnID string) error {
	tokens, err := s.srv.Tokens.List()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(tokens, func(t mcp.Token) bool { return t.ID == tokenID })
	if i < 0 {
		return errors.New("token not found")
	}
	d, err := s.st.Data()
	if err != nil {
		return err
	}
	v := d.VPN(vpnID)
	switch {
	case v == nil:
		return errors.New("VPN profile not found")
	case v.Tier == store.VPNTierSystem:
		return errors.New("a system-wide VPN profile changes the whole computer's network, so an MCP client always asks before it connects one")
	case v.Tier == store.VPNTierExternal:
		return errors.New("an external VPN profile connects in its own app, so an MCP client cannot connect it")
	}
	if err := s.widen(ctx, "Let "+tokens[i].Name+" connect "+v.Name+" without asking?",
		fmt.Sprintf("The MCP client %s will connect and disconnect the VPN profile %s (%s) without asking, until the profile's type, gateway or tier changes.", tokens[i].Name, v.Name, v.Gateway)); err != nil {
		return err
	}
	e := store.VPNAllowEntry{Token: tokenID, Profile: vpnID, Type: v.Type, Gateway: v.Gateway, Tier: v.Tier}
	return s.st.EditLocal(func(l *store.Local) error {
		l.MCP.VPNAllow = append(slices.DeleteFunc(l.MCP.VPNAllow, func(x store.VPNAllowEntry) bool { return x.Token == tokenID && x.Profile == vpnID }), e)
		return nil
	})
}

// RemoveVPNAllow takes a VPN entry off; that only makes more ask.
func (s *MCPService) RemoveVPNAllow(tokenID, vpnID string) error {
	return s.st.EditLocal(func(l *store.Local) error {
		l.MCP.VPNAllow = slices.DeleteFunc(l.MCP.VPNAllow, func(x store.VPNAllowEntry) bool { return x.Token == tokenID && x.Profile == vpnID })
		return nil
	})
}

// PickFolder sets the MCP folder from a dialog Go opens. The data folder,
// ~/.ssh, a folder inside either and a folder that holds either are
// refused. A cancelled dialog changes nothing and returns "".
func (s *MCPService) PickFolder() (string, error) {
	dir, err := s.pick("Choose the folder MCP clients may copy files to and from")
	if err != nil || dir == "" {
		return "", err
	}
	ssh, err := s.sshDir()
	if err != nil {
		return "", err
	}
	if err := mcp.CheckFolder(dir, s.st.Dir(), ssh); err != nil {
		return "", err
	}
	return dir, s.st.EditLocal(func(l *store.Local) error {
		l.MCP.Folder = dir
		return nil
	})
}

// mcpVPNs is the VPN service as MCP's tools reach it. A connect starts
// nothing silently: the tools ask first (KTD6).
type mcpVPNs struct{ creds *HostCredentials }

func (m *mcpVPNs) State(id string) mcp.VPNLive {
	var l mcp.VPNLive
	sts := m.creds.vpns.VPNStates()
	if i := slices.IndexFunc(sts, func(st VPNState) bool { return st.ID == id }); i >= 0 {
		l = mcp.VPNLive{Status: sts[i].Status, Reason: sts[i].Reason, Need: sts[i].Need}
	}
	if by, ok := m.creds.vpns.connectedBy(id); ok {
		l.By = cmp.Or(by, "user")
	}
	return l
}

func (m *mcpVPNs) Connect(ctx context.Context, id, principal string) (mcp.VPNLive, error) {
	if m.CanDrive(id) {
		err := m.creds.vpns.connectClientAs(ctx, id, principal)
		return m.State(id), err
	}
	_, err := m.creds.vpns.connectAs(ctx, id, principal)
	return m.State(id), err
}

func (m *mcpVPNs) Disconnect(id, principal string) error {
	if m.CanDrive(id) {
		return m.creds.vpns.disconnectClientAs(context.Background(), id, principal)
	}
	return m.creds.vpns.disconnectAs(id, principal)
}

// CanDrive reports whether an external profile can be connected from here,
// through its app's command line.
func (m *mcpVPNs) CanDrive(id string) bool { return m.creds.vpns.canDriveClient(id) }

// mcpHosts is saved SSH hosts as the MCP gate reaches them: each request
// dials or joins the host's lease with no prompts and releases it when done.
type mcpHosts struct {
	conns *ConnService
	creds *HostCredentials
	pool  sftp.Pool
}

func (m *mcpHosts) Label(id string) (string, error) {
	d, err := m.creds.st.Data()
	if err != nil {
		return "", err
	}
	h := d.Host(id)
	switch {
	case h == nil:
		return "", errors.New("no saved host has that id; call list_connections")
	case h.KindName() != "ssh":
		return "", errors.New("MCP acts on saved SSH hosts only")
	}
	return hostLabel(d, id), nil
}

// Secrets is what the host's output may not carry to a client: its saved
// password, key-store key and post-login answers (KTD31).
func (m *mcpHosts) Secrets(id string) []string {
	var out []string
	if c, err := m.creds.Lookup(id); err == nil {
		out = append(out, c.Password, string(c.Key))
	}
	return append(out, m.conns.tools.knownSecrets(id)...)
}

// with runs fn on a lease on host, released afterwards.
func (m *mcpHosts) with(ctx context.Context, host string, fn func(*sshx.Lease) error) error {
	l, err := m.conns.promptlessLease(ctx, host)
	if err != nil {
		return err
	}
	defer l.Release()
	return fn(l)
}

func (m *mcpHosts) Run(ctx context.Context, host, command string, stdout, stderr io.Writer) (code int, err error) {
	err = m.with(ctx, host, func(l *sshx.Lease) error {
		code, err = leaseExec(ctx, l, command, stdout, stderr)
		return err
	})
	return code, err
}

func (m *mcpHosts) sftp(ctx context.Context, l *sshx.Lease) (*sftp.Conn, error) {
	cl, err := l.Live(ctx)
	if err != nil {
		return nil, err
	}
	c, err := m.pool.Get(cl.Client)
	if errors.Is(err, sftp.ErrNoSFTP) {
		return nil, errors.New("this host has no SFTP, so MCP cannot copy files there")
	}
	return c, err
}

func (m *mcpHosts) List(ctx context.Context, host, dir string) (out []agentexec.RemoteEntry, err error) {
	err = m.with(ctx, host, func(l *sshx.Lease) error {
		c, err := m.sftp(ctx, l)
		if err != nil {
			return err
		}
		es, err := c.List(dir)
		for _, e := range es {
			out = append(out, agentexec.RemoteEntry{Name: e.Name, Path: e.Path, IsDir: e.IsDir, Size: e.Size, Mtime: e.Mtime})
		}
		return err
	})
	return out, err
}

func (m *mcpHosts) Upload(ctx context.Context, host, remote string, r io.Reader) error {
	return m.with(ctx, host, func(l *sshx.Lease) error {
		c, err := m.sftp(ctx, l)
		if err != nil {
			return err
		}
		u, err := c.Create(remote)
		if err != nil {
			return err
		}
		stop := context.AfterFunc(ctx, func() { u.Abort() })
		defer stop()
		if _, err := io.Copy(u, r); err != nil {
			u.Abort()
			return err
		}
		return u.Commit()
	})
}

func (m *mcpHosts) Download(ctx context.Context, host, remote string, w io.Writer) error {
	return m.with(ctx, host, func(l *sshx.Lease) error {
		c, err := m.sftp(ctx, l)
		if err != nil {
			return err
		}
		f, err := c.Open(remote)
		if err != nil {
			return err
		}
		defer f.Close()
		stop := context.AfterFunc(ctx, func() { f.Close() })
		defer stop()
		_, err = io.Copy(w, f)
		return err
	})
}
