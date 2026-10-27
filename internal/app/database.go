package app

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"hostbay/internal/ai"
	"hostbay/internal/apperr"
	"hostbay/internal/database"
	"hostbay/internal/store"
	"hostbay/internal/tlstrust"
)

// DatabaseService opens database hosts in Go and serves their tabs
// (R56, KTD23). Connect returns a database ID and each request a query ID,
// both minted here; unknown IDs get nothing. Passwords stay in Go, and
// every error and query state is scrubbed of them before it leaves.
type DatabaseService struct {
	creds *HostCredentials
	conns *ConnService
	certs *tlstrust.Store
	dir   string
	emit  func(name string, data any)
	// pickOpen and pickSave run Go's own file dialogs, so the page never
	// names a path that is opened or written.
	pickOpen func() (string, error)
	pickSave func() (string, error)
	reg      *database.Registry

	mu  sync.Mutex
	dbs map[string]*dbSession
}

type dbSession struct {
	engine database.Engine
	secret string
	ctx    context.Context // ends when the tab closes, with every query in it
	cancel context.CancelFunc
	kind   string // the host's kind, such as postgres
}

// DatabaseInfo is an open database: its ID, and the capabilities the tab
// shows controls for (database.Capabilities).
type DatabaseInfo struct {
	DatabaseID   string   `json:"databaseId"`
	Capabilities []string `json:"capabilities"`
}

// DatabaseTree is one page of an object tree level.
type DatabaseTree struct {
	Nodes []database.Node `json:"nodes"`
	More  bool            `json:"more"`
}

// DatabaseTable is a table opened from the tree: the query showing its
// rows, and its key when its rows can be edited (nil: read-only).
type DatabaseTable struct {
	QueryID string          `json:"queryId"`
	Table   *database.Table `json:"table"`
}

// FilePick is a file chosen in DatabasePickFile's dialog: its path, shown
// only, and the one-use token SaveHost takes in place of a path.
type FilePick struct {
	Token string `json:"token"`
	Path  string `json:"path"`
}

func NewDatabaseService(creds *HostCredentials, conns *ConnService, certs *tlstrust.Store, dir string) *DatabaseService {
	s := &DatabaseService{creds: creds, conns: conns, certs: certs, dir: dir, emit: emitEvent,
		pickOpen: pickDatabaseFile, pickSave: pickSaveFile, dbs: map[string]*dbSession{}}
	s.reg = database.NewRegistry(func(st database.State) {
		st.Message = database.Scrub(st.Message, s.secrets()...)
		s.emit(QueryStateEvent(st.QueryID), st)
	})
	return s
}

func pickDatabaseFile() (string, error) {
	return application.Get().Dialog.OpenFile().CanChooseFiles(true).SetTitle("Choose a SQLite database").PromptForSingleSelection()
}

func pickSaveFile() (string, error) {
	return application.Get().Dialog.SaveFile().CanCreateDirectories(true).SetMessage("Save the full value").PromptForSingleSelection()
}

// DatabaseConnect opens a database host and returns its database ID. A
// network database is reached only through the host's jump host or proxy,
// never around them (KTD24), and in its TLS mode (KTD25): an unknown
// certificate fails with HostKeyUnknown (details.kind "<kind>Cert", such as "redisCert" or "mssqlCert"),
// answered with DatabaseTrustCert; a changed one with HostKeyChanged and a
// removeId for DatabaseRemovePin. Either way no password was sent. A
// missing or refused password is SecretNeeded.
func (s *DatabaseService) DatabaseConnect(ctx context.Context, hostID string) (DatabaseInfo, error) {
	c, err := s.creds.Lookup(hostID)
	if err != nil {
		return DatabaseInfo{}, err
	}
	e, err := s.open(ctx, c.Host, c.Password, s.conns.databaseDial)
	if err != nil {
		return DatabaseInfo{}, withHostID(s.openErr(err, c.Host, c.Password), hostID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	id := newRequestID()
	s.mu.Lock()
	s.dbs[id] = &dbSession{engine: e, secret: c.Password, ctx: ctx, cancel: cancel, kind: c.Host.KindName()}
	s.mu.Unlock()
	s.creds.touch(hostID)
	caps := database.Capabilities(e)
	if slices.Contains(sqlKinds, c.Host.KindName()) {
		caps = append(caps, "askSql")
	}
	return DatabaseInfo{DatabaseID: id, Capabilities: caps}, nil
}

// sqlKinds are the database kinds whose tab can turn a question into SQL
// (R64).
var sqlKinds = []string{"postgres", "mysql", "mssql", "oracle", "sqlite", "rqlite"}

// maxSchemaTables caps the schema a question to SQL sends.
const maxSchemaTables = 200

// schemaTimeout bounds the catalog walk; what it read by then is sent.
var schemaTimeout = 30 * time.Second

// schemaNames lists dbID's tables and views with their column names, up
// to 200 tables, from the object tree's own catalog queries. It reads no
// row and runs no request of the user's.
func (s *DatabaseService) schemaNames(ctx context.Context, dbID string) (string, []ai.SQLTable, error) {
	d, err := s.session(dbID)
	if err != nil {
		return "", nil, err
	}
	if !slices.Contains(sqlKinds, d.kind) {
		return "", nil, errors.New("this database does not take SQL")
	}
	ctx, cancel := context.WithTimeout(ctx, schemaTimeout)
	defer cancel()
	var tables []ai.SQLTable
	var walk func(path []string) error
	walk = func(path []string) error {
		for off := 0; len(tables) < maxSchemaTables; {
			nodes, more, err := d.engine.Children(ctx, path, off)
			if err != nil {
				return err
			}
			for _, n := range nodes {
				if len(tables) >= maxSchemaTables {
					return nil
				}
				switch {
				case n.Kind == "table" || n.Kind == "view":
					cols, _, err := d.engine.Children(ctx, n.Path, 0)
					if err != nil {
						return err
					}
					t := ai.SQLTable{Name: strings.Join(n.Path, ".")}
					for _, c := range cols {
						t.Columns = append(t.Columns, c.Name)
					}
					tables = append(tables, t)
				case n.HasChildren:
					if err := walk(n.Path); err != nil {
						return err
					}
				}
			}
			if !more || len(nodes) == 0 {
				return nil
			}
			off += len(nodes)
		}
		return nil
	}
	if err := walk(nil); err != nil && (ctx.Err() == nil || len(tables) == 0) {
		return "", nil, s.pageErr(err)
	}
	return d.kind, tables, nil
}

// DatabaseTest checks a SQL server or rqlite host from the editor's unsaved form,
// signing in and out, the way TestConnection checks an SSH host (R53): the
// form's typed password, or the saved one only while the form still
// matches what it was sealed to (KTD6). Step "database" is an error the
// database itself returned.
func (s *DatabaseService) DatabaseTest(ctx context.Context, host Host) (ConnTest, error) {
	if err := checkDatabaseHost(host); err != nil {
		return ConnTest{}, err
	}
	d, err := s.creds.st.Data()
	if err != nil {
		return ConnTest{}, err
	}
	h, err := testHost(d, host)
	if err != nil {
		return ConnTest{}, err
	}
	if !slices.Contains([]string{"postgres", "mysql", "mssql", "oracle", "rqlite"}, h.KindName()) {
		return ConnTest{}, errors.New("test connection checks PostgreSQL, MySQL, SQL Server, Oracle and rqlite hosts")
	}
	c, err := s.creds.unseal(d, h, store.Deref(host.Password))
	if err != nil {
		return testResult(err), nil
	}
	// The jump chain is dialled on its own for the test, not shared.
	dial := func(h store.Host, port int) database.Dial {
		return allowlisted(h, port, func(ctx context.Context) (net.Conn, error) { return s.conns.testTCP(ctx, d, &h, port) })
	}
	e, err := s.open(ctx, c.Host, c.Password, dial)
	if err != nil {
		r := testResult(s.openErr(err, c.Host, c.Password))
		if r.Error != nil && r.Error.Code == apperr.DatabaseError {
			r.Step = "database"
		}
		return r, nil
	}
	e.Close()
	return ConnTest{Step: "ok"}, nil
}

// databasePort is h's port, or its engine's default.
func databasePort(h store.Host) int {
	if def := map[string]int{"postgres": 5432, "redis": 6379, "mongodb": 27017, "mysql": 3306, "mssql": 1433, "oracle": 1521}[h.KindName()]; h.Port == 0 && def != 0 {
		return def
	}
	return h.Port
}

// databaseTarget is the host name and port h's connections go to, which
// its pins are keyed by: an Elasticsearch or rqlite host's from its URL.
func databaseTarget(h store.Host) (string, int, error) {
	if h.KindName() != "elasticsearch" && h.KindName() != "rqlite" {
		return h.Hostname, databasePort(h), nil
	}
	u, err := url.Parse(store.Deref(h.URL))
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", 0, errors.New("enter the node's http(s) URL")
	}
	port := map[string]int{"https": 443, "http": 80}[u.Scheme]
	if u.Port() != "" {
		if port, err = strconv.Atoi(u.Port()); err != nil {
			return "", 0, errors.New("the URL's port is not valid")
		}
	}
	return u.Hostname(), port, nil
}

// open opens h's engine, dialling through dial.
func (s *DatabaseService) open(ctx context.Context, h store.Host, password string, dial func(store.Host, int) database.Dial) (database.Engine, error) {
	switch h.KindName() {
	case "sqlite":
		if h.File == nil || *h.File == "" {
			return nil, errors.New("choose this host's SQLite database file in its settings")
		}
		return database.OpenSQLite(ctx, *h.File)
	case "postgres":
		port := databasePort(h)
		return database.OpenPostgres(ctx, database.PGConfig{
			Host: h.Hostname, Port: port, User: h.User, Password: password, Database: store.Deref(h.Database),
			TLS: h.DatabaseTLS(), Dial: dial(h, port),
			// Pinned by host and real target, whatever the jump host or proxy.
			Trust: func(try func(*tls.Config) error) (*tls.Config, error) {
				return s.certs.Capture("postgres", h.ID, h.Hostname, port, try)
			},
			NoClientCert: filepath.Join(s.dir, "no-client-certificate", "postgresql.crt"),
		})
	case "redis", "mongodb", "elasticsearch", "mysql", "mssql", "oracle", "rqlite":
		host, port, err := databaseTarget(h)
		if err != nil {
			return nil, err
		}
		// Pinned by host and real target, whatever the jump host or proxy.
		trust := func(try func(*tls.Config) error) (*tls.Config, error) {
			return s.certs.Capture(h.KindName(), h.ID, host, port, try)
		}
		th := h
		th.Hostname = host // an Elasticsearch or rqlite host is dialled at its URL's host
		c := database.ServerConfig{Host: host, Port: port, User: h.User, Password: password, Database: store.Deref(h.Database),
			TLS: h.DatabaseTLS(), Dial: dial(th, port), Trust: trust}
		switch h.KindName() {
		case "mysql":
			return database.OpenMySQL(ctx, c)
		case "mssql":
			return database.OpenMSSQL(ctx, c)
		case "oracle":
			return database.OpenOracle(ctx, c)
		case "rqlite":
			c.URL = store.Deref(h.URL)
			return database.OpenRqlite(ctx, c)
		case "redis":
			n := 0
			if db := store.Deref(h.Database); db != "" {
				if n, err = strconv.Atoi(db); err != nil {
					return nil, errors.New("the Redis database must be a number")
				}
			}
			return database.OpenRedis(ctx, database.RedisConfig{Host: host, Port: port, User: h.User, Password: password, DB: n,
				TLS: h.DatabaseTLS(), Dial: dial(th, port), Trust: trust})
		case "mongodb":
			return database.OpenMongo(ctx, database.MongoConfig{Host: host, Port: port, User: h.User, Password: password,
				AuthDB: store.Deref(h.Database), TLS: h.DatabaseTLS(), Dial: dial(th, port), Trust: trust})
		}
		return database.OpenElasticsearch(ctx, database.ESConfig{URL: store.Deref(h.URL), User: h.User, Password: password, Dial: dial(th, port), Trust: trust})
	}
	return nil, errors.New("this host is not a database host")
}

// openErr is what the page sees of a failed open: a changed certificate
// gets the ID that removes its pin, a refused sign-in asks for the
// password again, and every error loses the password (KTD26).
func (s *DatabaseService) openErr(err error, h store.Host, password string) error {
	var ae *apperr.Error
	switch {
	case errors.As(err, &ae) && ae.Code == apperr.HostKeyChanged && ae.Details["kind"] == h.KindName()+"Cert":
		host, port, _ := databaseTarget(h)
		ae.With("removeId", s.certs.RemovalRequest(h.KindName(), h.ID, host, port))
		if h.KindName() == "mssql" {
			ae.Message += " A SQL Server without a certificate of its own makes a new one each time it restarts; give it a certificate so its pin stays valid."
		}
	case database.Category(err) == "auth":
		return apperr.New(apperr.SecretNeeded, "The database refused the user name or password; enter the password again.").
			With("reason", "wrong").With("detail", database.Scrub(err.Error(), password))
	}
	return database.AppError(err, password)
}

// session returns the open database dbID.
func (s *DatabaseService) session(dbID string) (*dbSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.dbs[dbID]; d != nil {
		return d, nil
	}
	return nil, errors.New("this database connection has closed; connect again")
}

// secrets are the passwords of every open database.
func (s *DatabaseService) secrets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.dbs))
	for _, d := range s.dbs {
		out = append(out, d.secret)
	}
	return out
}

// pageErr is err as the page may see it.
func (s *DatabaseService) pageErr(err error) error { return database.AppError(err, s.secrets()...) }

// DatabaseTree lists up to database.TreePage children of path (none: the
// top level) from offset.
func (s *DatabaseService) DatabaseTree(ctx context.Context, dbID string, path []string, offset int) (DatabaseTree, error) {
	d, err := s.session(dbID)
	if err != nil {
		return DatabaseTree{}, err
	}
	nodes, more, err := d.engine.Children(ctx, path, offset)
	if err != nil {
		return DatabaseTree{}, s.pageErr(err)
	}
	if nodes == nil {
		nodes = []database.Node{}
	}
	return DatabaseTree{Nodes: nodes, More: more}, nil
}

// DatabaseQuery runs text (the selection or the whole editor) as one
// request and returns its query ID at once. Its state arrives as
// query-state events and its rows from DatabaseNextPage.
func (s *DatabaseService) DatabaseQuery(dbID, text string) (string, error) {
	d, err := s.session(dbID)
	if err != nil {
		return "", err
	}
	return s.reg.Start(d.ctx, dbID, func(ctx context.Context) (database.Results, error) { return d.engine.Query(ctx, text) }), nil
}

// DatabaseOpenTable shows a table from the tree. Its key comes from the
// database's own catalog, so DatabaseEditRow never trusts the page for it.
func (s *DatabaseService) DatabaseOpenTable(ctx context.Context, dbID string, path []string) (DatabaseTable, error) {
	d, err := s.session(dbID)
	if err != nil {
		return DatabaseTable{}, err
	}
	// Row editors open tables and keys, document editors collections and
	// indices.
	ed, ok := d.engine.(interface {
		EditableTable(context.Context, []string) (*database.Table, error)
		SelectTable([]string) string
	})
	if !ok {
		return DatabaseTable{}, errors.New("this database cannot open tables")
	}
	t, err := ed.EditableTable(ctx, path)
	if err != nil {
		return DatabaseTable{}, s.pageErr(err)
	}
	text := ed.SelectTable(path)
	id := s.reg.Start(d.ctx, dbID, func(ctx context.Context) (database.Results, error) { return d.engine.Query(ctx, text) })
	return DatabaseTable{QueryID: id, Table: t}, nil
}

// DatabaseNextPage returns a query's next page of rows.
func (s *DatabaseService) DatabaseNextPage(ctx context.Context, queryID string) (database.Page, error) {
	p, err := s.reg.Next(ctx, queryID)
	if err != nil {
		return database.Page{}, s.pageErr(err)
	}
	return p, nil
}

// DatabaseCancel cancels a running query through the database's own
// cancel; its state then reports "cancelled".
func (s *DatabaseService) DatabaseCancel(queryID string) error {
	s.reg.Cancel(queryID)
	return nil
}

// DatabaseCloseQuery closes a query's results. Unknown IDs are already
// closed.
func (s *DatabaseService) DatabaseCloseQuery(queryID string) error {
	s.reg.Close(queryID)
	return nil
}

// DatabaseEditRow changes one row of a table opened with
// DatabaseOpenTable, by the table's own key, in a transaction that commits
// only when exactly one row changed (KTD23).
func (s *DatabaseService) DatabaseEditRow(ctx context.Context, dbID string, edit database.RowEdit) error {
	d, err := s.session(dbID)
	if err != nil {
		return err
	}
	ed, ok := d.engine.(database.RowEditor)
	if !ok {
		return errors.New("this database cannot edit rows")
	}
	return s.pageErr(ed.EditRow(ctx, edit))
}

// documents is dbID's document editor.
func (s *DatabaseService) documents(dbID string) (database.DocumentEditor, error) {
	d, err := s.session(dbID)
	if err != nil {
		return nil, err
	}
	ed, ok := d.engine.(database.DocumentEditor)
	if !ok {
		return nil, errors.New("this database cannot edit documents")
	}
	return ed, nil
}

// DatabaseDocument returns, as JSON, the whole document of a collection or
// index opened with DatabaseOpenTable that its "_id" cell names, for the
// document view: a cell shown in the grid may be cut.
func (s *DatabaseService) DatabaseDocument(ctx context.Context, dbID string, path []string, id string) (string, error) {
	ed, err := s.documents(dbID)
	if err != nil {
		return "", err
	}
	doc, err := ed.Document(ctx, path, id)
	return doc, s.pageErr(err)
}

// DatabaseSaveDocument replaces that document with doc, by its _id; it
// fails when the document no longer exists or doc names another _id.
func (s *DatabaseService) DatabaseSaveDocument(ctx context.Context, dbID string, path []string, id, doc string) error {
	ed, err := s.documents(dbID)
	if err != nil {
		return err
	}
	return s.pageErr(ed.EditDocument(ctx, path, id, doc))
}

// DatabaseSaveCell writes the full value of a cut or binary cell from the
// query's last page to a file the user chooses in Go's dialog. It reports
// false when the user cancelled.
func (s *DatabaseService) DatabaseSaveCell(queryID string, result, row, col int) (bool, error) {
	path, err := s.pickSave()
	if err != nil || path == "" {
		return false, err
	}
	f, err := os.Create(path)
	if err != nil {
		return false, err
	}
	err = s.reg.SaveCell(queryID, result, row, col, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return false, s.pageErr(err)
	}
	return true, nil
}

// DatabaseClose closes a database tab: its queries, then its connection.
// Unknown IDs are already closed.
func (s *DatabaseService) DatabaseClose(dbID string) error {
	s.mu.Lock()
	d := s.dbs[dbID]
	delete(s.dbs, dbID)
	s.mu.Unlock()
	if d == nil {
		return nil
	}
	s.reg.CloseOwner(dbID)
	d.cancel()
	return d.engine.Close()
}

// DatabaseTrustCert answers the HostKeyUnknown prompt (details.kind
// "<kind>Cert") carrying requestID; accepting pins the certificate.
func (s *DatabaseService) DatabaseTrustCert(requestID string, accept bool) error {
	return s.certs.Answer(requestID, accept)
}

// DatabaseRemovePin removes the pinned certificate a HostKeyChanged
// refusal named by removeId, after the user typed the refused host name
// and passed OS verification (KTD25); the next connect asks to pin again.
func (s *DatabaseService) DatabaseRemovePin(ctx context.Context, removeID, typedHost string) error {
	return s.certs.RemovePin(ctx, removeID, typedHost, s.creds.vault.Gate)
}

// DatabasePickFile asks, in Go's dialog, for a SQLite file and returns it
// with the token SaveHost takes. A cancelled dialog returns no token.
func (s *DatabaseService) DatabasePickFile() (FilePick, error) {
	path, err := s.pickOpen()
	if err != nil || path == "" {
		return FilePick{}, err
	}
	return FilePick{Token: addFilePick(path), Path: path}, nil
}

// filePicks holds the files chosen in DatabasePickFile's dialog until
// SaveHost takes them, each once and within pendingTrustTTL.
var filePicks = struct {
	sync.Mutex
	m map[string]filePick
}{m: map[string]filePick{}}

type filePick struct {
	path    string
	expires time.Time
}

func addFilePick(path string) string {
	id := newRequestID()
	now := time.Now()
	filePicks.Lock()
	defer filePicks.Unlock()
	for k, p := range filePicks.m { // picks never saved would pile up
		if now.After(p.expires) {
			delete(filePicks.m, k)
		}
	}
	filePicks.m[id] = filePick{path: path, expires: now.Add(pendingTrustTTL)}
	return id
}

func takeFilePick(token string) (string, bool) {
	filePicks.Lock()
	defer filePicks.Unlock()
	p, ok := filePicks.m[token]
	delete(filePicks.m, token)
	return p.path, ok && !time.Now().After(p.expires)
}
