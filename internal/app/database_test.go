package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"

	"hostbay/internal/apperr"
	"hostbay/internal/database"
	"hostbay/internal/database/dbtest"
	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx/proxytest"
	"hostbay/internal/store"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
)

const dbPassword = "db-Secret-42!"

// eventLog records emitted events.
type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (l *eventLog) emit(name string, data any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, _ := data.(database.State)
	l.ev = append(l.ev, name+" "+s.State+" "+s.Message)
}

func (l *eventLog) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.ev, "\n")
}

func newDatabaseService(t *testing.T, knownHosts ...string) (*DatabaseService, *HostService, string, *eventLog) {
	t.Helper()
	c, s, dir := connWithKnownHosts(t, knownHosts...)
	d := NewDatabaseService(c.creds, c, tlstrust.New(dir), dir)
	ev := &eventLog{}
	d.emit = ev.emit
	return d, s, dir, ev
}

func pgForm(srv *dbtest.PG, tls string) Host {
	pw := dbPassword
	return Host{Alias: "pg", Hostname: srv.Host, Port: srv.Port, User: "app", Kind: new("postgres"), TLS: &tls, Database: new("app"), Password: &pw}
}

// A PostgreSQL host is reached through its proxy only; a proxy that
// refuses fails the connect and the server sees no connection (KTD24).
func TestDatabaseConnectGoesThroughTheProxyOnly(t *testing.T) {
	srv := dbtest.NewPG(t, nil)
	d, s, _, _ := newDatabaseService(t)
	p := proxytest.SOCKS5(t, "", "")
	px, err := s.SaveProxy(Proxy{Name: "office", Type: "socks5", Host: p.Host, Port: p.Port})
	if err != nil {
		t.Fatal(err)
	}
	form := pgForm(srv, "none")
	form.ProxyID = &px.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	// The fake server refuses every password: reaching it proves the path.
	if _, err := d.DatabaseConnect(context.Background(), h.ID); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("err = %v, want the refused password to ask again", err)
	}
	if !slices.Contains(p.Targets(), srv.Addr()) || len(srv.Passwords()) != 1 {
		t.Fatalf("proxy carried %v; server saw %d passwords", p.Targets(), len(srv.Passwords()))
	}

	down, port := proxytest.Down(t)
	dead, err := s.SaveProxy(Proxy{Name: "dead", Type: "http", Host: down, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	h.ProxyID = &dead.ID
	if _, err := s.SaveHost(h); err != nil {
		t.Fatal(err)
	}
	before := srv.Conns()
	if _, err := d.DatabaseConnect(context.Background(), h.ID); apperr.CodeOf(err) != apperr.ProxyFailed {
		t.Fatalf("err = %v, want ProxyFailed", err)
	}
	if srv.Conns() != before {
		t.Fatal("the server saw a direct connection")
	}
}

// A PostgreSQL host behind a jump host is dialled from the jump host, by
// a name only it resolves; with the jump host down nothing reaches the
// server.
func TestDatabaseConnectGoesThroughTheJumpHostOnly(t *testing.T) {
	srv := dbtest.NewPG(t, nil)
	jump := sftptest.New(t, "jump-pw")
	jump.Resolve("pg.internal", srv.Addr())
	d, s, _, _ := newDatabaseService(t, jump.KnownHostsLine())
	jhost, jport, _ := net.SplitHostPort(jump.Addr)
	jp, _ := strconv.Atoi(jport)
	auth, jpw := "password", "jump-pw"
	j, err := s.SaveHost(Host{Alias: "bastion", Hostname: jhost, Port: jp, User: "u", AuthType: &auth, Password: &jpw})
	if err != nil {
		t.Fatal(err)
	}
	form := pgForm(srv, "none")
	form.Hostname, form.JumpHostID = "pg.internal", &j.ID
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.DatabaseConnect(context.Background(), h.ID); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("err = %v, want the refused password to ask again", err)
	}
	if want := net.JoinHostPort("pg.internal", strconv.Itoa(srv.Port)); !slices.Contains(jump.Dialed(), want) {
		t.Fatalf("jump host dialled %v, want %s", jump.Dialed(), want)
	}

	// The down case uses an address this computer reaches directly, so a
	// direct fallback would show as a server connection.
	down, port := proxytest.Down(t)
	j.Hostname, j.Port, j.Password = down, port, &jpw // a moved host keeps no sealed password
	if _, err := s.SaveHost(j); err != nil {
		t.Fatal(err)
	}
	reachable := pgForm(srv, "none")
	reachable.Alias, reachable.JumpHostID = "pg-reachable", &j.ID
	r, err := s.SaveHost(reachable)
	if err != nil {
		t.Fatal(err)
	}
	before := srv.Conns()
	for _, id := range []string{h.ID, r.ID} {
		if _, err := d.DatabaseConnect(context.Background(), id); err == nil {
			t.Fatal("connected with the jump host down")
		}
	}
	if srv.Conns() != before {
		t.Fatal("the server saw a direct connection")
	}
}

// The dial function opens only the host's own address; any other, such
// as a server-sent redirect, is refused by name and never dialled. A host
// name matches in any case, as the MongoDB driver lowercases it.
func TestDatabaseDialRefusesEveryOtherAddress(t *testing.T) {
	h := store.Host{Alias: "pg", Hostname: "DB.Internal"}
	dialled := 0
	dial := allowlisted(h, 5432, func(context.Context) (net.Conn, error) {
		dialled++
		return nil, errors.New("test dial")
	})
	for _, c := range [][2]string{{"tcp", "10.9.9.9:5432"}, {"tcp", "db.internal:5433"}, {"unix", "db.internal:5432"}, {"tcp", "db.internal.evil:5432"}} {
		_, err := dial(context.Background(), c[0], c[1])
		if err == nil || !strings.Contains(err.Error(), c[1]) {
			t.Fatalf("dial %v: %v", c, err)
		}
	}
	if dialled != 0 {
		t.Fatal("a refused address was dialled")
	}
	if _, err := dial(context.Background(), "tcp", "db.internal:5432"); err == nil || err.Error() != "test dial" || dialled != 1 {
		t.Fatalf("own address: %v", err)
	}
}

// With no saved password the connect asks for it and sends nothing (R29).
func TestPostgresWithoutAPasswordAsksForIt(t *testing.T) {
	srv := dbtest.NewPG(t, nil)
	d, s, _, _ := newDatabaseService(t)
	form := pgForm(srv, "none")
	form.Password = nil
	h, err := s.SaveHost(form)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.DatabaseConnect(context.Background(), h.ID)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.SecretNeeded || ae.Details["reason"] != "missing" || ae.Details["hostId"] != h.ID {
		t.Fatalf("err = %v", err)
	}
	if srv.Conns() != 0 {
		t.Fatal("the server was contacted without a password")
	}
}

// Testing an edited host's new address without retyping the password
// sends no password there (KTD6).
func TestDatabaseTestNeverSendsTheSavedPasswordElsewhere(t *testing.T) {
	srv, other := dbtest.NewPG(t, nil), dbtest.NewPG(t, nil)
	d, s, _, _ := newDatabaseService(t)
	h, err := s.SaveHost(pgForm(srv, "none"))
	if err != nil {
		t.Fatal(err)
	}
	h.Password = nil
	if r, _ := d.DatabaseTest(context.Background(), h); r.Step != "auth" || len(srv.Passwords()) != 1 || srv.Passwords()[0] != dbPassword {
		t.Fatalf("unchanged host: %+v, passwords %v", r, srv.Passwords())
	}
	h.Port = other.Port
	r, _ := d.DatabaseTest(context.Background(), h)
	if r.Step != "auth" || r.Error == nil || r.Error.Code != apperr.SecretNeeded {
		t.Fatalf("moved host: %+v", r)
	}
	if other.Conns() != 0 {
		t.Fatal("the new address was contacted")
	}
}

// The page cannot set a SQLite host's path: only a file chosen in the
// dialog Go opened, by its one-use token.
func TestSQLitePathComesOnlyFromTheFileDialog(t *testing.T) {
	d, s, _, _ := newDatabaseService(t)
	chosen := filepath.Join(t.TempDir(), "app.db")
	d.pickOpen = func() (string, error) { return chosen, nil }
	form := Host{Alias: "local", Kind: new("sqlite"), File: new("/etc/passwd")}
	if _, err := s.SaveHost(form); err == nil {
		t.Fatal("saved a SQLite host with a path the page sent")
	}
	pick, err := d.DatabasePickFile()
	if err != nil || pick.Path != chosen || pick.Token == "" {
		t.Fatalf("pick = %+v, %v", pick, err)
	}
	form.FilePick = &pick.Token
	h, err := s.SaveHost(form)
	if err != nil || h.File == nil || *h.File != chosen {
		t.Fatalf("saved = %+v, %v", h.File, err)
	}
	h.File = new("/etc/passwd")
	if h, err = s.SaveHost(h); err != nil || *h.File != chosen {
		t.Fatalf("resaved = %v, %v", *h.File, err)
	}
	h.FilePick = &pick.Token
	if _, err := s.SaveHost(h); err == nil {
		t.Fatal("a file token worked twice")
	}
}

// A changed certificate is refused with a removal ID; removal needs the
// typed host name and verification, then the next connect asks again.
func TestDatabasePinRemovalThroughTheService(t *testing.T) {
	cert := tlstest.Cert(t)
	srv := dbtest.NewPG(t, &cert)
	d, s, _, _ := newDatabaseService(t)
	h, err := s.SaveHost(pgForm(srv, "verify"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := func(want apperr.Code) *apperr.Error {
		t.Helper()
		_, err := d.DatabaseConnect(context.Background(), h.ID)
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code != want {
			t.Fatalf("err = %v, want %s", err, want)
		}
		return ae
	}
	ae := prompt(apperr.HostKeyUnknown)
	if ae.Details["kind"] != "postgresCert" {
		t.Fatalf("prompt = %+v", ae.Details)
	}
	if err := d.DatabaseTrustCert(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	prompt(apperr.SecretNeeded) // trusted: the sign-in is reached
	next := tlstest.Cert(t)
	srv.SetCert(&next)
	ae = prompt(apperr.HostKeyChanged)
	signIns := len(srv.Passwords())
	if ae.Details["removeId"] == "" {
		t.Fatalf("no removal ID: %+v", ae.Details)
	}
	if err := d.DatabaseRemovePin(context.Background(), ae.Details["removeId"], "wrong.example"); err == nil {
		t.Fatal("removed with a mistyped host name")
	}
	prompt(apperr.HostKeyChanged)
	ae = prompt(apperr.HostKeyChanged)
	if err := d.DatabaseRemovePin(context.Background(), ae.Details["removeId"], srv.Host); err != nil {
		t.Fatal(err)
	}
	prompt(apperr.HostKeyUnknown)
	if len(srv.Passwords()) != signIns {
		t.Fatal("a password was sent to the changed certificate")
	}
}

// A SQLite host browses, queries, edits a row and saves a cell through
// the service, and its IDs stop working once closed.
func TestSQLiteThroughTheService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT); INSERT INTO users VALUES (1, 'ann'), (2, 'bob');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	d, s, _, ev := newDatabaseService(t)
	d.pickOpen = func() (string, error) { return path, nil }
	pick, _ := d.DatabasePickFile()
	h, err := s.SaveHost(Host{Alias: "local", Kind: new("sqlite"), FilePick: &pick.Token})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	info, err := d.DatabaseConnect(ctx, h.ID)
	if err != nil || !slices.Contains(info.Capabilities, "rowEdit") || !slices.Contains(info.Capabilities, "askSql") {
		t.Fatalf("connect = %+v, %v", info, err)
	}
	tree, err := d.DatabaseTree(ctx, info.DatabaseID, nil, 0)
	if err != nil || len(tree.Nodes) != 1 || tree.Nodes[0].Name != "users" {
		t.Fatalf("tree = %+v, %v", tree, err)
	}
	tbl, err := d.DatabaseOpenTable(ctx, info.DatabaseID, tree.Nodes[0].Path)
	if err != nil || tbl.Table == nil || !slices.Equal(tbl.Table.Key, []string{"id"}) {
		t.Fatalf("table = %+v, %v", tbl, err)
	}
	p, err := d.DatabaseNextPage(ctx, tbl.QueryID)
	if err != nil || len(p.Rows) != 2 {
		t.Fatalf("page = %+v, %v", p, err)
	}
	two, carl := "2", "carl"
	if err := d.DatabaseEditRow(ctx, info.DatabaseID, database.RowEdit{Path: tbl.Table.Path, Key: map[string]*string{"id": &two}, Set: map[string]*string{"name": &carl}}); err != nil {
		t.Fatal(err)
	}
	q, err := d.DatabaseQuery(info.DatabaseID, "SELECT name FROM users ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if p, err = d.DatabaseNextPage(ctx, q); err != nil || p.Rows[0][0].Text != "ann" || p.Rows[1][0].Text != "carl" {
		t.Fatalf("after edit = %+v, %v", p.Rows, err)
	}
	if !strings.Contains(ev.all(), QueryStateEvent(q)+" done") {
		t.Fatalf("events:\n%s", ev.all())
	}
	long, err := d.DatabaseQuery(info.DatabaseID, "SELECT printf('%.*c', 70000, 'x')")
	if err != nil {
		t.Fatal(err)
	}
	if p, err = d.DatabaseNextPage(ctx, long); err != nil || !p.Rows[0][0].Cut {
		t.Fatalf("long cell = %v, %v", p.Rows[0][0].Size, err)
	}
	out := filepath.Join(t.TempDir(), "cell.txt")
	d.pickSave = func() (string, error) { return out, nil }
	if saved, err := d.DatabaseSaveCell(long, 0, 0, 0); err != nil || !saved {
		t.Fatalf("save = %v, %v", saved, err)
	}
	if b, _ := os.ReadFile(out); len(b) != 70000 {
		t.Fatalf("saved %d bytes", len(b))
	}
	if !strings.Contains(ev.all(), QueryStateEvent(q)+" done") {
		t.Fatalf("events:\n%s", ev.all())
	}
	if err := d.DatabaseClose(info.DatabaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DatabaseQuery(info.DatabaseID, "SELECT 1"); err == nil {
		t.Fatal("a closed database ID still answers")
	}
	if _, err := d.DatabaseNextPage(ctx, q); err == nil {
		t.Fatal("a closed database's cursor still answers")
	}
}

// A server error echoing the password reaches the page without it, and no
// Hostbay file holds the password in plain (KTD26, KTD6).
func TestDatabasePasswordNeverLeavesInPlain(t *testing.T) {
	srv := dbtest.NewPG(t, nil)
	srv.Fail("XX000", "internal error near "+dbPassword)
	d, s, dir, _ := newDatabaseService(t)
	h, err := s.SaveHost(pgForm(srv, "none"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.DatabaseConnect(context.Background(), h.ID)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.DatabaseError {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), dbPassword) || strings.Contains(ae.Details["detail"], dbPassword) || !strings.Contains(err.Error(), "internal error near") {
		t.Fatalf("err = %v", err)
	}
	if len(srv.Passwords()) != 1 {
		t.Fatal("the sign-in was not reached")
	}
	err = filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err == nil && bytes.Contains(b, []byte(dbPassword)) {
			t.Errorf("%s holds the password in plain", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A SQL Server restarted with a new self-generated certificate is refused,
// saying why; after the verified removal the next connect asks to pin
// again, and no LOGIN7 reached the changed server (KTD25).
func TestMSSQLRestartedWithANewCertificate(t *testing.T) {
	old, cur := tlstest.Cert(t), tlstest.Cert(t)
	srv := dbtest.NewTDS(t, &old)
	d, s, _, _ := newDatabaseService(t)
	pw := dbPassword
	h, err := s.SaveHost(Host{Alias: "ms", Hostname: srv.Host, Port: srv.Port, User: "app", Kind: new("mssql"), Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	prompt := func(want apperr.Code) *apperr.Error {
		t.Helper()
		_, err := d.DatabaseConnect(context.Background(), h.ID)
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code != want || ae.Details["kind"] != "mssqlCert" {
			t.Fatalf("err = %v, want %s", err, want)
		}
		return ae
	}
	if err := d.DatabaseTrustCert(prompt(apperr.HostKeyUnknown).Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	srv.SetCert(&cur)
	logins := srv.Logins()
	ae := prompt(apperr.HostKeyChanged)
	if !strings.Contains(ae.Message, "makes a new one each time it restarts") || ae.Details["removeId"] == "" {
		t.Fatalf("refusal = %+v", ae)
	}
	if err := d.DatabaseRemovePin(context.Background(), ae.Details["removeId"], srv.Host); err != nil {
		t.Fatal(err)
	}
	prompt(apperr.HostKeyUnknown)
	if srv.Logins() != logins {
		t.Fatal("LOGIN7 reached the server whose certificate changed")
	}
}

// An rqlite host connects to its URL's host and port, which the dial
// function allows, and to nothing else.
func TestRqliteConnectsToItsURL(t *testing.T) {
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		w.Write([]byte(`{"results":[{"columns":["1"],"types":["integer"],"values":[[1]]}]}`))
	}))
	defer srv.Close()
	d, s, _, _ := newDatabaseService(t)
	h, err := s.SaveHost(Host{Alias: "rq", Kind: new("rqlite"), URL: new(srv.URL)})
	if err != nil {
		t.Fatal(err)
	}
	info, err := d.DatabaseConnect(context.Background(), h.ID)
	if err != nil || seen.Load() != 1 || !slices.Contains(info.Capabilities, "rowEdit") {
		t.Fatalf("connect = %+v, %v; requests %d", info, err, seen.Load())
	}
	d.DatabaseClose(info.DatabaseID)
	if _, err := s.SaveHost(Host{Alias: "rq", Kind: new("rqlite"), URL: new("ftp://x")}); err == nil {
		t.Fatal("saved an rqlite host with a URL it cannot connect to")
	}
}

// A DBeaver connection that used an SSH tunnel imports with the
// missing-proxy marker: connecting it fails, and the database never sees
// a connection, so it is never reached directly.
func TestImportedTunnelledDatabaseNeverDialsDirectly(t *testing.T) {
	srv := dbtest.NewPG(t, nil)
	d, s, _, _ := newDatabaseService(t)
	dir := t.TempDir()
	sources := `{"connections":{"pg-1":{"provider":"postgresql","name":"tunnelled","configuration":{"host":"` + srv.Host + `","port":` + strconv.Itoa(srv.Port) +
		`,"database":"app","user":"app","handlers":{"ssh_tunnel":{"type":"TUNNEL","enabled":true,"properties":{"host":"bastion.example","port":22}}}}}}}`
	if err := os.WriteFile(filepath.Join(dir, "data-sources.json"), []byte(sources), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportFrom("dbeaver", dir, ""); err != nil {
		t.Fatal(err)
	}
	hosts, _ := s.ListHosts()
	if len(hosts) != 1 || hosts[0].ProxyID == nil {
		t.Fatalf("hosts = %+v", hosts)
	}
	// Entering the password, as the connect prompt asks, still goes nowhere.
	pw := dbPassword
	hosts[0].Password = &pw
	if _, err := s.SaveHost(hosts[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DatabaseConnect(context.Background(), hosts[0].ID); apperr.CodeOf(err) != apperr.ProxyFailed {
		t.Fatalf("err = %v, want ProxyFailed", err)
	}
	if srv.Conns() != 0 {
		t.Fatal("the database saw a direct connection")
	}
}
