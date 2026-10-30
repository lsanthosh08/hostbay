package backup

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

// newEnv opens an empty store and a vault over an in-memory keychain.
func newEnv(t *testing.T) (*store.Store, *secrets.Vault) {
	t.Helper()
	st, v, _ := newEnvDir(t)
	return st, v
}

func newEnvDir(t *testing.T) (*store.Store, *secrets.Vault, string) {
	t.Helper()
	keyring.MockInit()
	dir := filepath.Join(t.TempDir(), "Hostbay")
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	v := secrets.New(st, nil)
	// Linux refuses to seal before a master password exists (KTD7).
	if err := v.SetMasterPassword("test master password"); err != nil {
		t.Fatal(err)
	}
	return st, v, dir
}

func data(t *testing.T, st *store.Store) *store.Data {
	t.Helper()
	d, err := st.Data()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const (
	webPassword = "Sample-Pass-9!"
	keyFile     = "-----BEGIN OPENSSH PRIVATE KEY-----\nsample-key-body\n-----END OPENSSH PRIVATE KEY-----\n"
)

// seed saves a password host, a host using a key-store key, a folder, a
// script and a bookmark.
func seed(t *testing.T, st *store.Store, v *secrets.Vault) {
	t.Helper()
	err := st.Edit(func(d *store.Data) error {
		f := d.AddFolder(store.Folder{Name: "Prod"})
		web := store.Host{Meta: store.Meta{ID: store.NewID()}, Alias: "web", Hostname: "10.1.1.1", Port: 22, User: "root", FolderID: &f.ID}
		sealed, err := v.Seal([]byte(webPassword), web.SecretAAD())
		if err != nil {
			return err
		}
		web.Password = sealed
		d.AddHost(web)
		id := store.NewID()
		sealedKey, err := v.Seal([]byte(keyFile), store.KeyAAD(id))
		if err != nil {
			return err
		}
		k, err := d.AddKey(store.Key{Meta: store.Meta{ID: id}, Name: "deploy", Data: sealedKey})
		if err != nil {
			return err
		}
		d.AddHost(store.Host{Alias: "db", Hostname: "db.internal", Port: 2222, User: "deploy", IdentityFile: store.KeyRefPrefix + k.Name})
		d.SetScripts([]store.Script{{Label: "Uptime", Script: "uptime"}})
		d.SetBookmarks([]string{"/srv"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedExportRoundTrips(t *testing.T) {
	st, v := newEnv(t)
	seed(t, st, v)
	out, err := Export(data(t, st), v, "", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(out, []byte(magic)) {
		t.Fatalf("export starts %q, want the Hostbay magic", out[:8])
	}
	for _, leak := range []string{webPassword, "sample-key-body", base64.StdEncoding.EncodeToString([]byte(keyFile)), "10.1.1.1"} {
		if bytes.Contains(out, []byte(leak)) {
			t.Fatalf("encrypted export contains %q", leak)
		}
	}

	b, err := Read(out, "wrong passphrase")
	if err == nil || b.Hosts != nil {
		t.Fatalf("wrong passphrase: Read = %+v, %v; want no output and an error", b, err)
	}
	b, err = Read(out, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}

	st2, v2 := newEnv(t)
	err = st2.Edit(func(d *store.Data) error {
		n, _, err := Restore(d, v2, b)
		if n != 2 {
			t.Errorf("restored %d hosts, want 2", n)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	d := data(t, st2)
	var web, db *store.Host
	for i := range d.Hosts.Items {
		switch h := &d.Hosts.Items[i]; h.Alias {
		case "web":
			web = h
		case "db":
			db = h
		}
	}
	if web == nil || db == nil {
		t.Fatalf("restored hosts = %+v", d.Hosts.Items)
	}
	pw, err := v2.Open(web.Password, web.SecretAAD())
	if err != nil || string(pw) != webPassword {
		t.Fatalf("restored password = %q, %v", pw, err)
	}
	if web.FolderID == nil || d.Folder(*web.FolderID).Name != "Prod" {
		t.Fatalf("web folder = %v", web.FolderID)
	}
	k := d.KeyByName(strings.TrimPrefix(db.IdentityFile, store.KeyRefPrefix))
	if k == nil {
		t.Fatalf("db key %q not restored", db.IdentityFile)
	}
	if got, err := v2.Open(k.Data, store.KeyAAD(k.ID)); err != nil || string(got) != keyFile {
		t.Fatalf("restored key = %q, %v", got, err)
	}
	if s := d.ScriptList(); len(s) != 1 || s[0].Label != "Uptime" {
		t.Fatalf("scripts = %+v", s)
	}
}

func TestPlainExportHasNoSecrets(t *testing.T) {
	st, v := newEnv(t)
	seed(t, st, v)
	out, err := Export(data(t, st), v, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{webPassword, "sample-key-body", base64.StdEncoding.EncodeToString([]byte(keyFile)), "enc:v1:", `"password"`, `"keys"`, store.KeyRefPrefix} {
		if bytes.Contains(out, []byte(leak)) {
			t.Fatalf("plain export contains %q:\n%s", leak, out)
		}
	}
	b, err := Read(out, "")
	if err != nil || len(b.Hosts) != 2 {
		t.Fatalf("plain export reads back as %d hosts, %v", len(b.Hosts), err)
	}
}

func TestExportOneHost(t *testing.T) {
	st, v := newEnv(t)
	seed(t, st, v)
	d := data(t, st)
	var dbID string
	for _, h := range d.Hosts.Items {
		if h.Alias == "db" {
			dbID = h.ID
		}
	}
	out, err := Export(d, v, dbID, "pass phrase")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Read(out, "pass phrase")
	if err != nil || len(b.Hosts) != 1 || b.Hosts[0].Alias != "db" || len(b.Folders) != 0 || len(b.Keys) != 1 {
		t.Fatalf("one-host export = %+v, %v", b, err)
	}
}

// storeFiles reads every file in the store folder dir.
// A rule's answer travels in an encrypted backup and is sealed for the
// restored host; a plain backup keeps the rule without it, and neither
// holds the device-local history or logs (KTD39).
func TestPostLoginRulesBackUpWithTheirAnswerOnlyEncrypted(t *testing.T) {
	const answer = "Sample-Answer-7?"
	st, v := newEnv(t)
	var rid string
	err := st.Edit(func(d *store.Data) error {
		h := d.AddHost(store.Host{Alias: "web", Hostname: "web.example", Port: 22, User: "root"})
		r := store.PostLoginRule{Meta: store.Meta{ID: store.NewID()}, HostID: h.ID, Steps: []store.PostLoginStep{{Expect: "Password:", Kind: store.ReplySecret}}}
		sealed, err := v.Seal([]byte(answer), store.PostLoginAAD(r.ID, &h))
		r.Answer = sealed
		rid = d.PutPostLogin(r).ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddHistory("web", "history-only-sentinel"); err != nil {
		t.Fatal(err)
	}
	for _, pass := range []string{"pass phrase", ""} {
		out, err := Export(data(t, st), v, "", pass)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte(answer)) || bytes.Contains(out, []byte("history-only-sentinel")) {
			t.Fatalf("the backup holds a secret or history in readable form:\n%s", out)
		}
		b, err := Read(out, pass)
		if err != nil || b.SchemaVersion != 4 || len(b.PostLogin) != 1 {
			t.Fatalf("schema %d, rules %+v, %v", b.SchemaVersion, b.PostLogin, err)
		}
		st2, v2 := newEnv(t)
		if err := st2.Edit(func(d *store.Data) error { _, _, err := Restore(d, v2, b); return err }); err != nil {
			t.Fatal(err)
		}
		d2 := data(t, st2)
		r := d2.PostLoginRule(rid)
		if r == nil {
			t.Fatal("the rule was not restored")
		}
		if pass == "" {
			if r.Answer != "" {
				t.Fatal("a plain backup restored an answer")
			}
			continue
		}
		got, err := v2.Open(r.Answer, store.PostLoginAAD(r.ID, d2.Host(r.HostID)))
		if string(got) != answer || err != nil {
			t.Fatalf("restored answer = %q, %v", got, err)
		}
	}
}

func storeFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.Type().IsRegular() {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			out[e.Name()] = b
		}
	}
	return out
}

// A proxy and the host that uses it survive an encrypted backup and
// restore, the proxy's password re-sealed on the new store; a plain backup
// keeps the proxy without its password (KTD22).
func TestProxiesRoundTripThroughBackup(t *testing.T) {
	const proxyPassword = "Proxy-Pass-4!"
	st, v := newEnv(t)
	var hostID string
	err := st.Edit(func(d *store.Data) error {
		p := store.Proxy{Meta: store.Meta{ID: store.NewID()}, Name: "office", Type: "http", Host: "proxy.example", Port: 3128, User: "pu"}
		sealed, err := v.Seal([]byte(proxyPassword), p.SecretAAD())
		p.Password = sealed
		d.AddProxy(p)
		hostID = d.AddHost(store.Host{Alias: "web", Hostname: "10.1.1.1", Port: 22, User: "root", ProxyID: &p.ID}).ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, one := range []string{"", hostID} {
		out, err := Export(data(t, st), v, one, "pass phrase")
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte(proxyPassword)) {
			t.Fatal("encrypted export holds the proxy password in the clear")
		}
		b, err := Read(out, "pass phrase")
		if err != nil || b.SchemaVersion != 2 {
			t.Fatalf("schema %d, %v; a build that drops proxies must refuse this backup", b.SchemaVersion, err)
		}
		st2, v2 := newEnv(t)
		if err := st2.Edit(func(d *store.Data) error { _, _, err := Restore(d, v2, b); return err }); err != nil {
			t.Fatal(err)
		}
		d := data(t, st2)
		if len(d.Proxies.Items) != 1 || len(d.Hosts.Items) != 1 {
			t.Fatalf("one=%q: restored %d proxies, %d hosts", one, len(d.Proxies.Items), len(d.Hosts.Items))
		}
		p, h := d.Proxies.Items[0], d.Hosts.Items[0]
		if h.ProxyID == nil || *h.ProxyID != p.ID {
			t.Fatalf("restored host names proxy %v, want %s", h.ProxyID, p.ID)
		}
		if pw, err := v2.Open(p.Password, p.SecretAAD()); err != nil || string(pw) != proxyPassword {
			t.Fatalf("restored proxy password = %q, %v", pw, err)
		}
	}

	out, err := Export(data(t, st), v, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("enc:v1:")) || bytes.Contains(out, []byte(proxyPassword)) || !bytes.Contains(out, []byte("proxy.example")) {
		t.Fatalf("plain export:\n%s", out)
	}
}

// A backup holding a PostgreSQL host is schema version 3, so a Release 2
// build that would drop its database fields refuses it; a plain backup
// keeps the host without its password, readable or sealed, and neither the
// store nor the backups hold it readable (secret scan).
func TestDatabaseHostBackupIsVersion3WithoutItsPasswordInPlain(t *testing.T) {
	const dbPassword = "Db-Pass-31!"
	st, v, dir := newEnvDir(t)
	pg, name := "postgres", "app"
	err := st.Edit(func(d *store.Data) error {
		h := store.Host{Meta: store.Meta{ID: store.NewID()}, Alias: "db", Hostname: "db.example", Port: 5432, User: "app", Kind: &pg, Database: &name}
		sealed, err := v.Seal([]byte(dbPassword), h.SecretAAD())
		h.Password = sealed
		d.AddHost(h)
		d.AddHost(store.Host{Alias: "ssh", Hostname: "10.1.1.1", Port: 22, User: "root"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, pass := range []string{"pass phrase", ""} {
		out, err := Export(data(t, st), v, "", pass)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte(dbPassword)) {
			t.Fatalf("export (passphrase %q) holds the database password readable", pass)
		}
		b, err := Read(out, pass)
		if err != nil || b.SchemaVersion != 3 {
			t.Fatalf("schema %d, %v; a build that drops database hosts must refuse this backup", b.SchemaVersion, err)
		}
		if pass == "" && (bytes.Contains(out, []byte("enc:v1:")) || !bytes.Contains(out, []byte("db.example"))) {
			t.Fatalf("plain export:\n%s", out)
		}
	}
	raw := []byte(dbPassword)
	for i := range raw {
		raw[i] ^= obfKey[i%len(obfKey)]
	}
	obf := base64.StdEncoding.EncodeToString(raw)
	for name, b := range storeFiles(t, dir) {
		if bytes.Contains(b, []byte(dbPassword)) || bytes.Contains(b, []byte(obf)) {
			t.Fatalf("%s holds the database password readable", name)
		}
	}
}

// A backup carries the synced settings and is schema version 4, so a build
// that would drop them refuses it. Device-local settings stay out of every
// backup (KTD37). Restore adds a group only where this computer has none,
// and never the ai group: a file must not weaken the AI execution mode.
func TestSettingsBackupIsVersion4WithoutLocalSettings(t *testing.T) {
	st, v := newEnv(t)
	err := st.Edit(func(d *store.Data) error {
		d.PutSettings(store.SettingsGroup{Meta: store.Meta{ID: store.GroupAppearance}, Theme: "light", FontSize: 15})
		d.PutSettings(store.SettingsGroup{Meta: store.Meta{ID: store.GroupAI}, ExecutionMode: store.AIDangerousOnly})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EditLocal(func(l *store.Local) error {
		l.HostLogging = map[string]bool{"local-only-sentinel": true}
		l.MCP = store.MCPSettings{Enabled: true, Folder: "/mcp-local-only-sentinel"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(st, "mcp-tokens.json", []map[string]string{{"name": "mcp-local-only-sentinel"}}); err != nil {
		t.Fatal(err)
	}
	for _, pass := range []string{"pass phrase", ""} {
		out, err := Export(data(t, st), v, "", pass)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Read(out, pass)
		if err != nil || b.SchemaVersion != 4 || len(b.Settings) != 2 {
			t.Fatalf("schema %d, settings %+v, %v", b.SchemaVersion, b.Settings, err)
		}
		if pass == "" && (bytes.Contains(out, []byte("local-only-sentinel")) || bytes.Contains(out, []byte("hostLogging")) || bytes.Contains(out, []byte(`"mcp`))) {
			t.Fatalf("plain export carries local settings:\n%s", out)
		}

		st2, v2 := newEnv(t)
		err = st2.Edit(func(d *store.Data) error {
			d.PutSettings(store.SettingsGroup{Meta: store.Meta{ID: store.GroupLanguage}, Language: "de"})
			_, _, err := Restore(d, v2, b)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		d2 := data(t, st2)
		if ap := d2.SettingsGroup(store.GroupAppearance); ap == nil || ap.Theme != "light" || ap.Revision != 1 {
			t.Fatalf("restored appearance = %+v", ap)
		}
		if lang := d2.SettingsGroup(store.GroupLanguage); lang.Language != "de" {
			t.Fatalf("restore overwrote the local language: %+v", lang)
		}
		if d2.SettingsGroup(store.GroupAI) != nil || d2.AIMode() != store.AIConfirmAll {
			t.Fatalf("restore brought in the AI mode: %q", d2.AIMode())
		}
	}
}

// A workspace comes back pointing at the local copy of each of its hosts,
// once only.
func TestWorkspaceRestoresOntoLocalHosts(t *testing.T) {
	st, v := newEnv(t)
	err := st.Edit(func(d *store.Data) error {
		web := d.AddHost(store.Host{Alias: "web", Hostname: "10.1.1.1", Port: 22, User: "root"})
		db := d.AddHost(store.Host{Alias: "db", Hostname: "10.1.1.2", Port: 22, User: "root"})
		d.PutWorkspace(store.Workspace{Name: "prod", Layout: store.WorkspaceNode{
			Dir: "row", Sizes: []float64{0.5, 0.5}, Children: []store.WorkspaceNode{{HostID: web.ID}, {HostID: db.ID}},
		}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Export(data(t, st), v, "", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Read(out, "")
	if err != nil || b.SchemaVersion != 4 || len(b.Workspaces) != 1 {
		t.Fatalf("schema %d, workspaces %+v, %v", b.SchemaVersion, b.Workspaces, err)
	}

	st2, v2 := newEnv(t)
	var local string
	for range 2 {
		err = st2.Edit(func(d *store.Data) error {
			if local == "" { // the same web host, saved here under its own ID
				local = d.AddHost(store.Host{Alias: "web", Hostname: "10.1.1.1", Port: 22, User: "root"}).ID
			}
			_, _, err := Restore(d, v2, b)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	d2 := data(t, st2)
	if len(d2.Workspaces.Items) != 1 {
		t.Fatalf("workspaces = %+v", d2.Workspaces.Items)
	}
	kids := d2.Workspaces.Items[0].Layout.Children
	if kids[0].HostID != local || d2.Host(kids[1].HostID) == nil || d2.Host(kids[1].HostID).Alias != "db" {
		t.Fatalf("restored panes = %+v", kids)
	}
}

// Skills and templates round-trip through both kinds of backup as version
// 4; a restore skips a name that is already saved here.
func TestSkillsRoundTripThroughBackup(t *testing.T) {
	st, v := newEnv(t)
	err := st.Edit(func(d *store.Data) error {
		d.PutSkill(store.Skill{Name: "deploy-check", Kind: store.SkillKind, Body: "Check <script>x</script>", AgentAuthored: true})
		d.PutSkill(store.Skill{Name: "explain", Kind: store.TemplateKind, Body: "Explain: {{input}}"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, pass := range []string{"pass phrase", ""} {
		out, err := Export(data(t, st), v, "", pass)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Read(out, pass)
		if err != nil || b.SchemaVersion != 4 || len(b.Skills) != 2 {
			t.Fatalf("schema %d, skills %+v, %v", b.SchemaVersion, b.Skills, err)
		}
		st2, v2 := newEnv(t)
		err = st2.Edit(func(d *store.Data) error {
			d.PutSkill(store.Skill{Name: "explain", Kind: store.TemplateKind, Body: "mine"})
			_, _, err := Restore(d, v2, b)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		d2 := data(t, st2)
		dc, ex := d2.SkillNamed("deploy-check"), d2.SkillNamed("explain")
		if len(d2.Skills.Items) != 2 || dc == nil || dc.Body != "Check <script>x</script>" || !dc.AgentAuthored || ex.Body != "mine" {
			t.Fatalf("restored skills = %+v", d2.Skills.Items)
		}
	}
}

// A host exported alone carries no jump host, so a restore elsewhere must
// not leave it to connect directly: whatever its kind, it gets the
// missing-proxy marker and fails to connect until a route is chosen (KTD24).
func TestRestoredHostWithAMissingJumpHostNeverConnectsDirectly(t *testing.T) {
	kube, u := "kubernetes", "https://10.0.0.9:6443"
	for _, h := range []store.Host{
		{Alias: "inner", Hostname: "10.0.0.8", Port: 22, User: "root"},
		{Alias: "cluster", Hostname: "10.0.0.9", Port: 6443, Kind: &kube, URL: &u},
	} {
		st, v := newEnv(t)
		var id string
		err := st.Edit(func(d *store.Data) error {
			j := d.AddHost(store.Host{Alias: "bastion", Hostname: "192.0.2.5", Port: 22, User: "jump"})
			h.JumpHostID = &j.ID
			id = d.AddHost(h).ID
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := Export(data(t, st), v, id, "pass phrase")
		if err != nil {
			t.Fatal(err)
		}
		b, err := Read(out, "pass phrase")
		if err != nil {
			t.Fatal(err)
		}
		st2, v2 := newEnv(t)
		var notes []string
		if err := st2.Edit(func(d *store.Data) error { _, notes, err = Restore(d, v2, b); return err }); err != nil {
			t.Fatal(err)
		}
		r := data(t, st2).Hosts.Items
		if len(r) != 1 || r[0].Jumped() || !r[0].Proxied() || !strings.HasPrefix(*r[0].ProxyID, "import-missing-") {
			t.Fatalf("%s restored as %+v, want the missing-proxy marker", h.Alias, r)
		}
		// The user is told which host waits for a route, or it just fails.
		if len(notes) != 1 || !strings.HasPrefix(notes[0], h.Alias+": ") || !strings.Contains(notes[0], "jump host") {
			t.Fatalf("%s restored with notes %q, want one naming it", h.Alias, notes)
		}
	}
}

// A VPN profile and the host that uses it survive an encrypted backup as
// bundle 5, so a build that would drop the profile and connect around it
// refuses the file; the profile's secrets are re-sealed on the new store,
// and a host is pointed at a matching profile already saved there. A plain
// backup keeps the profile without its secrets, so it asks for them (KTD4).
func TestVPNsRoundTripThroughBackup(t *testing.T) {
	const vpnKey = "Test-WG-Key-7!"
	secret, _ := json.Marshal(store.VPNSecrets{PrivateKey: vpnKey})
	newVPN := func() store.VPN {
		return store.VPN{Meta: store.Meta{ID: store.NewID()}, Name: "office", Type: "wireguard", Tier: store.VPNTierApp, Gateway: "vpn.example", Port: 51820, User: "vu", Tags: []string{"project-a"}, Routes: []string{"10.10.0.0/16"}}
	}
	st, v := newEnv(t)
	var hostID, vpnID string
	err := st.Edit(func(d *store.Data) error {
		p := newVPN()
		sealed, err := v.Seal(secret, p.SecretAAD())
		p.Secret = sealed
		vpnID = d.AddVPN(p).ID
		hostID = d.AddHost(store.Host{Alias: "web", Hostname: "10.10.1.1", Port: 22, User: "root", VPNID: &vpnID}).ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, one := range []string{"", hostID} {
		out, err := Export(data(t, st), v, one, "pass phrase")
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out, []byte(vpnKey)) {
			t.Fatal("encrypted export holds the VPN secret in the clear")
		}
		b, err := Read(out, "pass phrase")
		if err != nil || b.SchemaVersion != 5 {
			t.Fatalf("schema %d, %v; a build that drops VPN profiles must refuse this backup", b.SchemaVersion, err)
		}
		st2, v2 := newEnv(t)
		if err := st2.Edit(func(d *store.Data) error { _, _, err := Restore(d, v2, b); return err }); err != nil {
			t.Fatal(err)
		}
		d := data(t, st2)
		if len(d.VPNs.Items) != 1 || len(d.Hosts.Items) != 1 {
			t.Fatalf("one=%q: restored %d profiles, %d hosts", one, len(d.VPNs.Items), len(d.Hosts.Items))
		}
		p, h := d.VPNs.Items[0], d.Hosts.Items[0]
		if h.VPNID == nil || *h.VPNID != p.ID || len(p.Tags) != 1 || len(p.Routes) != 1 {
			t.Fatalf("restored host names VPN %v, want %s; profile %+v", h.VPNID, p.ID, p)
		}
		if raw, err := v2.Open(p.Secret, p.SecretAAD()); err != nil || string(raw) != string(secret) {
			t.Fatalf("restored VPN secret = %q, %v", raw, err)
		}
	}

	// A matching profile saved here under another ID is reused, not added.
	out, err := Export(data(t, st), v, hostID, "pass phrase")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Read(out, "pass phrase")
	if err != nil {
		t.Fatal(err)
	}
	st3, v3 := newEnv(t)
	var localID string
	if err := st3.Edit(func(d *store.Data) error { localID = d.AddVPN(newVPN()).ID; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := st3.Edit(func(d *store.Data) error { _, _, err := Restore(d, v3, b); return err }); err != nil {
		t.Fatal(err)
	}
	d := data(t, st3)
	if h := d.Hosts.Items[0]; len(d.VPNs.Items) != 1 || localID == vpnID || h.VPNID == nil || *h.VPNID != localID {
		t.Fatalf("host names VPN %v, want the local %s; %d profiles", h.VPNID, localID, len(d.VPNs.Items))
	}

	out, err = Export(data(t, st), v, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("enc:v1:")) || bytes.Contains(out, []byte(vpnKey)) || !bytes.Contains(out, []byte("vpn.example")) {
		t.Fatalf("plain export:\n%s", out)
	}
	b, err = Read(out, "")
	if err != nil {
		t.Fatal(err)
	}
	st4, v4 := newEnv(t)
	if err := st4.Edit(func(d *store.Data) error { _, _, err := Restore(d, v4, b); return err }); err != nil {
		t.Fatal(err)
	}
	if p := data(t, st4).VPNs.Items; len(p) != 1 || p[0].Secret != "" {
		t.Fatalf("plain restore = %+v, want the profile without its secret", p)
	}
}

// A host whose VPN profile is not in the backup keeps naming it, so it
// fails to connect rather than connect around the VPN, and the user is told.
func TestRestoredHostWithAMissingVPNNeverConnectsDirectly(t *testing.T) {
	gone := "gone-vpn"
	b := Bundle{SchemaVersion: 5, Hosts: []store.Host{{Alias: "inner", Hostname: "10.10.0.8", Port: 22, User: "root", VPNID: &gone}}}
	st, v := newEnv(t)
	var notes []string
	var err error
	if err := st.Edit(func(d *store.Data) error { _, notes, err = Restore(d, v, b); return err }); err != nil {
		t.Fatal(err)
	}
	if h := data(t, st).Hosts.Items[0]; h.VPNID == nil || *h.VPNID != gone {
		t.Fatalf("restored host = %+v, want it still naming the missing profile", h)
	}
	if len(notes) != 1 || !strings.HasPrefix(notes[0], "inner: ") || !strings.Contains(notes[0], "VPN") {
		t.Fatalf("notes = %q, want one naming the host", notes)
	}
}
