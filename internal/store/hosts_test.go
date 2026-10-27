package store

import (
	"reflect"
	"slices"
	"testing"
)

func edit(t *testing.T, s *Store, fn func(d *Data) error) *Data {
	t.Helper()
	if err := s.Edit(fn); err != nil {
		t.Fatal(err)
	}
	d, err := s.Data()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTrashRestoreAndPurgeLeavesTombstone(t *testing.T) {
	s := openTestStore(t)
	var id string
	edit(t, s, func(d *Data) error {
		id = d.PutHost(Host{Alias: "web", Hostname: "w", Port: 22, User: "u"}).ID
		return nil
	})
	d := edit(t, s, func(d *Data) error { return d.TrashHost(id) })
	if h := d.Host(id); h == nil || h.DeletedAt == "" || h.Revision != 2 {
		t.Fatalf("trashed host = %+v", h)
	}
	d = edit(t, s, func(d *Data) error { return d.RestoreHost(id) })
	if h := d.Host(id); h.DeletedAt != "" || h.Revision != 3 {
		t.Fatalf("restored host = %+v", h)
	}
	d = edit(t, s, func(d *Data) error { return d.PurgeHost(id) })
	if d.Host(id) != nil {
		t.Fatal("purged host still listed")
	}
	if ts := d.Hosts.Tombstones; len(ts) != 1 || ts[0].ID != id || !ts[0].Deleted || ts[0].Revision != 4 || ts[0].DeviceID != s.DeviceID() {
		t.Fatalf("tombstones = %+v", ts)
	}
}

func TestDeleteFolderReparentsOrTrashes(t *testing.T) {
	s := openTestStore(t)
	var top, mid, leaf, hostID string
	edit(t, s, func(d *Data) error {
		top = d.AddFolder(Folder{Name: "top"}).ID
		f, err := d.SaveFolder(Folder{Name: "mid", ParentID: &top})
		mid = f.ID
		if err != nil {
			return err
		}
		f, err = d.SaveFolder(Folder{Name: "leaf", ParentID: &mid})
		leaf = f.ID
		hostID = d.PutHost(Host{Alias: "h", FolderID: &mid}).ID
		return err
	})
	if err := s.Edit(func(d *Data) error {
		f := *d.Folder(top)
		f.ParentID = &leaf
		_, err := d.SaveFolder(f)
		return err
	}); err == nil {
		t.Fatal("moving a folder into its own descendant succeeded")
	}

	d := edit(t, s, func(d *Data) error { return d.DeleteFolder(mid, false) })
	if h := d.Host(hostID); h.FolderID == nil || *h.FolderID != top {
		t.Fatalf("host folder = %v, want top", h.FolderID)
	}
	if f := d.Folder(leaf); f.ParentID == nil || *f.ParentID != top {
		t.Fatalf("leaf parent = %v, want top", f.ParentID)
	}

	d = edit(t, s, func(d *Data) error { return d.DeleteFolder(top, true) })
	if len(d.Folders.Items) != 0 || len(d.Folders.Tombstones) != 3 {
		t.Fatalf("folders = %+v tombstones = %d", d.Folders.Items, len(d.Folders.Tombstones))
	}
	if h := d.Host(hostID); h.DeletedAt == "" || h.FolderID != nil {
		t.Fatalf("host = %+v, want trashed at top level", h)
	}
}

// Keeps unique_key_name's numbering from src-tauri/src/keys.rs.
func TestUniqueKeyName(t *testing.T) {
	d := &Data{}
	for _, n := range []string{"id_ed25519", "deploy.pem", "deploy-1.pem"} {
		d.Keys.Items = append(d.Keys.Items, Key{Name: n})
	}
	for in, want := range map[string]string{"id_ed25519": "id_ed25519-1", "deploy.pem": "deploy-2.pem", "new": "new"} {
		if got := d.UniqueKeyName(in); got != want {
			t.Errorf("UniqueKeyName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScriptsAndBookmarksDefaultUntilSaved(t *testing.T) {
	s := openTestStore(t)
	d, err := s.Data()
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ScriptList()) != len(defaultScripts) || len(d.BookmarkPaths()) != len(defaultBookmarks) {
		t.Fatal("unsaved scripts or bookmarks do not show the defaults")
	}
	var keep Script
	d = edit(t, s, func(d *Data) error {
		d.SetScripts([]Script{{Label: "a", Script: "echo a"}, {Label: "b", Script: "echo b"}})
		keep = d.Scripts.Items[0]
		d.SetBookmarks([]string{})
		return nil
	})
	if len(d.BookmarkPaths()) != 0 {
		t.Fatalf("bookmarks = %v, want none after saving an empty list", d.BookmarkPaths())
	}
	d = edit(t, s, func(d *Data) error {
		d.SetScripts([]Script{keep})
		return nil
	})
	if got := d.Scripts.Items; len(got) != 1 || got[0].Meta != keep.Meta {
		t.Fatalf("unchanged script was touched: %+v vs %+v", got, keep)
	}
	if len(d.Scripts.Tombstones) != 1 {
		t.Fatalf("removed script left %d tombstones, want 1", len(d.Scripts.Tombstones))
	}
}

func TestFileHostKindsAndFieldsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	str := func(v string) *string { return &v }
	yes := true
	want := map[string]Host{
		"ftp":    {Alias: "ftp", Hostname: "ftp.lan", Port: 990, User: "u", Kind: str("ftp"), TLS: str("implicit")},
		"smb":    {Alias: "smb", Hostname: "nas.lan", User: "u", Kind: str("smb"), Domain: str("CORP")},
		"webdav": {Alias: "dav", User: "u", Kind: str("webdav"), URL: str("https://dav.lan/remote.php/dav/files/u")},
		"s3":     {Alias: "s3", User: "AKID", Kind: str("s3"), URL: str("http://minio.lan:9000"), Bucket: str("bkt"), Region: str("eu-west-1"), PathStyle: &yes},
	}
	ids := map[string]string{}
	d := edit(t, s, func(d *Data) error { // edit reads the saved files back
		for k, h := range want {
			ids[k] = d.PutHost(h).ID
		}
		return nil
	})
	for k, w := range want {
		got := *d.Host(ids[k])
		got.Meta, w.Meta = Meta{}, Meta{}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("%s:\n got  %+v\n want %+v", k, got, w)
		}
	}
}

func TestSecretBindingKeepsOldKindsAndBindsHowFileKindsConnect(t *testing.T) {
	str := func(v string) *string { return &v }
	ssh := Host{Meta: Meta{ID: "id"}, Hostname: "h", Port: 22, User: "u"}
	if got := string(ssh.SecretAAD()); got != "id\x00h\x0022\x00u" {
		t.Fatalf("an SSH host's binding changed, so its saved password would stop opening: %q", got)
	}
	rdp := ssh
	rdp.Kind = str("rdp")
	if string(rdp.SecretAAD()) != string(ssh.SecretAAD()) {
		t.Fatal("an RDP host's binding changed")
	}

	ftps := Host{Meta: Meta{ID: "id"}, Hostname: "h", Port: 21, User: "u", Kind: str("ftp")}
	explicit := ftps
	explicit.TLS = str("explicit")
	plain := ftps
	plain.TLS = str("none")
	if string(ftps.SecretAAD()) != string(explicit.SecretAAD()) {
		t.Fatal("an unset FTP TLS mode is explicit TLS")
	}
	if string(ftps.SecretAAD()) == string(plain.SecretAAD()) || ftps.Plaintext() || !plain.Plaintext() {
		t.Fatal("a password saved for FTPS would open for plain FTP")
	}

	dav := Host{Meta: Meta{ID: "id"}, Kind: str("webdav"), URL: str("https://dav.lan/x")}
	davPlain := dav
	davPlain.URL = str("http://dav.lan/x")
	if string(dav.SecretAAD()) == string(davPlain.SecretAAD()) || dav.Plaintext() || !davPlain.Plaintext() {
		t.Fatal("a password saved for an https WebDAV URL would open for http")
	}

	vnc := Host{Meta: Meta{ID: "id"}, Hostname: "h", Port: 5900, User: "u", Kind: str("vnc")}
	vencrypt := vnc
	vencrypt.TLS = str("vencrypt")
	vncPlain := vnc
	vncPlain.TLS = str("none")
	if string(vnc.SecretAAD()) != string(vencrypt.SecretAAD()) || vnc.VNCSecurity() != "vencrypt" {
		t.Fatal("an unset VNC security mode is VeNCrypt")
	}
	if string(vnc.SecretAAD()) == string(vncPlain.SecretAAD()) || vnc.Plaintext() || !vncPlain.Plaintext() {
		t.Fatal("a password saved for VeNCrypt would open for plain VNC")
	}
	tunnelled := vncPlain
	tunnelled.JumpHostID = str("bastion")
	if tunnelled.Plaintext() {
		t.Fatal("plain VNC over a jump host's SSH connection is not unencrypted on the way")
	}

	spice := Host{Meta: Meta{ID: "id"}, Hostname: "h", Port: 5901, Kind: str("spice")}
	spiceTLS := spice
	spiceTLS.TLS = str("tls")
	spicePlain := spice
	spicePlain.TLS = str("none")
	if string(spice.SecretAAD()) != string(spiceTLS.SecretAAD()) || spice.SpiceSecurity() != "tls" || spice.Plaintext() {
		t.Fatal("an unset SPICE security mode is a TLS port")
	}
	if string(spice.SecretAAD()) == string(spicePlain.SecretAAD()) || !spicePlain.Plaintext() {
		t.Fatal("a password saved for a SPICE TLS port would open for a plain one")
	}
	spicePlain.JumpHostID = str("bastion")
	if spicePlain.Plaintext() {
		t.Fatal("plain SPICE over a jump host's SSH connection is not unencrypted on the way")
	}
}

// A database host's password is bound to its kind and TLS mode, so a switch
// to "none" leaves it unopenable; "none" is unencrypted even through a jump
// host, unless the database runs on the jump host itself (KTD25).
func TestDatabaseHostsBindTheirTLSModeAndStayOutOfFileKinds(t *testing.T) {
	str := func(v string) *string { return &v }
	pg := Host{Meta: Meta{ID: "id"}, Hostname: "db.lan", Port: 5432, User: "u", Kind: str("postgres"), Database: str("app")}
	verify := pg
	verify.TLS = str("verify")
	plain := pg
	plain.TLS = str("none")
	if pg.DatabaseTLS() != "verify" || string(pg.SecretAAD()) != string(verify.SecretAAD()) {
		t.Fatal("an unset database TLS mode is verify")
	}
	if string(pg.SecretAAD()) == string(plain.SecretAAD()) || pg.Plaintext() || !plain.Plaintext() {
		t.Fatal("a password saved for verified TLS would open for an unencrypted connection")
	}
	asSSH := pg
	asSSH.Kind = nil
	if string(pg.SecretAAD()) == string(asSSH.SecretAAD()) {
		t.Fatal("a database password would open for an SSH host at the same address")
	}
	plain.JumpHostID = str("bastion")
	if !plain.Plaintext() {
		t.Fatal("the hop from the jump host to the database is cleartext")
	}
	plain.Hostname = "127.0.0.1"
	if plain.Plaintext() {
		t.Fatal("a database on the jump host itself never leaves it")
	}
	if slices.Contains(FileKinds, "postgres") || slices.Contains(FileKinds, "sqlite") {
		t.Fatal("database kinds would forbid a jump host")
	}
	lite := Host{Kind: str("sqlite"), File: str("/tmp/x.db")}
	if lite.Plaintext() {
		t.Fatal("a local SQLite file sends nothing over a network")
	}
}

// A cluster's token is bound to its kind and server URL (KTD25, KTD28), so
// pointing the host at another server leaves it unopenable. A cluster is
// always https and never a database or file kind.
func TestKubernetesHostsBindTheirURL(t *testing.T) {
	str := func(v string) *string { return &v }
	k := Host{Meta: Meta{ID: "id"}, Alias: "prod", Hostname: "k8s.lan", Port: 6443, Kind: str("kubernetes"),
		URL: str("https://k8s.lan:6443"), CAData: str("ca"), TLSServerName: str("kubernetes"), Namespace: str("apps")}
	moved := k
	moved.URL = str("https://k8s.lan:6443/other")
	if string(k.SecretAAD()) == string(moved.SecretAAD()) {
		t.Fatal("a token would open after the cluster's URL changed")
	}
	asSSH := k
	asSSH.Kind = nil
	if string(k.SecretAAD()) == string(asSSH.SecretAAD()) {
		t.Fatal("a cluster token would open for an SSH host at the same address")
	}
	if k.Plaintext() || k.IsDatabase() || slices.Contains(FileKinds, "kubernetes") {
		t.Fatal("a cluster is https only and may use a jump host")
	}
}

// Redis and MongoDB bind their TLS mode as PostgreSQL does; Elasticsearch
// is addressed by URL, so its password is bound to the URL with its scheme
// and an http:// URL is unencrypted, unless it is the jump host's own
// loopback (KTD25).
func TestRedisMongoDBAndElasticsearchHostsBindHowTheyConnect(t *testing.T) {
	str := func(v string) *string { return &v }
	for _, k := range []string{"redis", "mongodb", "elasticsearch"} {
		if !slices.Contains(DatabaseKinds, k) || slices.Contains(FileKinds, k) {
			t.Fatalf("%s is not a database kind that may use a jump host", k)
		}
	}
	for _, k := range []string{"redis", "mongodb"} {
		h := Host{Meta: Meta{ID: "id"}, Hostname: "db.lan", Port: 6379, Kind: str(k)}
		plain := h
		plain.TLS = str("none")
		if h.Plaintext() || !plain.Plaintext() || string(h.SecretAAD()) == string(plain.SecretAAD()) {
			t.Fatalf("%s: a password saved for verified TLS would open for an unencrypted connection", k)
		}
		plain.JumpHostID, plain.Hostname = str("bastion"), "localhost"
		if plain.Plaintext() {
			t.Fatalf("%s on the jump host itself never leaves it", k)
		}
	}
	es := Host{Meta: Meta{ID: "id"}, Hostname: "es.lan", Port: 9200, Kind: str("elasticsearch"), URL: str("https://es.lan:9200")}
	plain := es
	plain.URL = str("http://es.lan:9200")
	if es.Plaintext() || !plain.Plaintext() || string(es.SecretAAD()) == string(plain.SecretAAD()) {
		t.Fatal("an Elasticsearch password saved for https:// would open for http://")
	}
	plain.JumpHostID, plain.URL = str("bastion"), str("http://127.0.0.1:9200")
	if plain.Plaintext() {
		t.Fatal("Elasticsearch on the jump host itself never leaves it")
	}
}

// MySQL, SQL Server and Oracle hosts bind their TLS mode as PostgreSQL does;
// an rqlite host connects to its URL, so it binds the URL, and http:// is
// its "none" (KTD25).
func TestSQLEngineHostsBindHowTheyConnect(t *testing.T) {
	str := func(v string) *string { return &v }
	for _, k := range []string{"mysql", "mssql", "oracle"} {
		h := Host{Meta: Meta{ID: "id"}, Hostname: "db.lan", Port: 1, User: "u", Kind: str(k)}
		plain := h
		plain.TLS = str("none")
		if !h.IsDatabase() || h.Plaintext() || !plain.Plaintext() || string(h.SecretAAD()) == string(plain.SecretAAD()) {
			t.Fatalf("%s: a password saved for verified TLS would open for an unencrypted connection", k)
		}
		if slices.Contains(FileKinds, k) {
			t.Fatalf("%s would forbid a jump host", k)
		}
	}
	rq := Host{Meta: Meta{ID: "id"}, User: "u", Kind: str("rqlite"), URL: str("https://rq.lan:4001")}
	plain := rq
	plain.URL = str("http://rq.lan:4001")
	if !rq.IsDatabase() || rq.Plaintext() || !plain.Plaintext() || string(rq.SecretAAD()) == string(plain.SecretAAD()) {
		t.Fatal("an rqlite password saved for https:// would open for http://")
	}
	plain.JumpHostID = str("bastion")
	if !plain.Plaintext() {
		t.Fatal("the hop from the jump host to rqlite is cleartext")
	}
	plain.URL = str("http://localhost:4001")
	if plain.Plaintext() {
		t.Fatal("rqlite on the jump host itself never leaves it")
	}
}
