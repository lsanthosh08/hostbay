package database

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode"
)

// ServerConfig is a MySQL, SQL Server, Oracle or rqlite host, from its
// structured fields only (KTD26). Database is Oracle's service name.
// rqlite connects to URL instead of Host and Port.
type ServerConfig struct {
	Host, User, Password, Database, URL string
	Port                                int
	TLS                                 string // "verify" or "none" (KTD25); rqlite: its URL's scheme
	Dial                                Dial
	// Trust finds the TLS config as tlstrust.Capture does: try connects
	// and signs in; it never runs unverified.
	Trust func(try func(*tls.Config) error) (*tls.Config, error)
}

// check refuses what no sign-in should send: no password, a port out of
// range, or a control character in a name.
func (c ServerConfig) check() error {
	if c.Password == "" && !(c.URL != "" && c.User == "") { // rqlite without a user signs in to nothing
		return errors.New("no password to sign in with")
	}
	for what, v := range map[string]string{"user name": c.User, "database name": c.Database} {
		if strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return fmt.Errorf("the %s contains a control character", what)
		}
	}
	if c.URL == "" && (c.Port < 1 || c.Port > 65535) {
		return errors.New("the port must be between 1 and 65535")
	}
	return nil
}

// dialect is what one SQL engine adds to the shared engine over
// database/sql (KTD23): identifier quoting, placeholders, catalog queries,
// type mapping and error decoding.
type dialect struct {
	// quote quotes one identifier.
	quote func(name string) string
	// placeholder is the nth (from 1) parameter's marker.
	placeholder func(n int) string
	// children is the catalog query for path's children; its rows are
	// kind, name and detail. limit and offset come last in args.
	children func(path []string) (query string, args []any)
	// key returns path's primary key, else a unique key over NOT NULL
	// columns, or nil.
	key func(ctx context.Context, db *sql.DB, path []string) ([]string, error)
	// tablePath reports whether path names a table or view.
	tablePath func(path []string) bool
	// binary reports a column type whose values are bytes, not text.
	binary func(dbType string) bool
	// category decodes the engine's own errors (Category for the rest).
	category func(error) string
	// dollarQuotes and nestedComments describe the lexer (PostgreSQL);
	// backslashEscapes and hashComments too (MySQL).
	dollarQuotes, nestedComments   bool
	backslashEscapes, hashComments bool
	// offsetFirst passes a catalog query's offset before its limit, for
	// OFFSET … FETCH bound by position (Oracle).
	offsetFirst bool
	// killer, when set, is called as each session connection opens and
	// returns what stops that session's running statement from another
	// connection of db, for a driver whose cancel only drops its
	// connection (MySQL).
	killer func(ctx context.Context, db *sql.DB, session *sql.Conn) (kill func(), err error)
}

// sqlEngine is the one SQL implementation (KTD23). User requests run on
// one session connection, held for the tab, so a BEGIN in one request and
// a COMMIT in the next work as they would in psql; the tree and row edits
// use the pool.
type sqlEngine struct {
	db      *sql.DB
	d       dialect
	onClose func()

	mu      sync.Mutex
	session *sql.Conn
	kill    func() // the session's killer (dialect.killer), or nil
	active  *sqlResults
}

func newSQLEngine(db *sql.DB, d dialect, onClose func()) *sqlEngine {
	return &sqlEngine{db: db, d: d, onClose: onClose}
}

// wrap gives err its category.
func (e *sqlEngine) wrap(err error) error { return categorized(err, e.d.category) }

// categorized gives err its category: the engine's own (category), else
// Category's.
func categorized(err error, category func(error) string) error {
	var de *Error
	if err == nil || errors.As(err, &de) {
		return err
	}
	c := category(err)
	if c == "" {
		c = Category(err)
	}
	return &Error{Category: c, Err: err}
}

func (e *sqlEngine) Children(ctx context.Context, path []string, offset int) ([]Node, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, CatalogTimeout)
	defer cancel()
	q, args := e.d.children(path)
	if q == "" {
		return []Node{}, false, nil
	}
	page := []any{TreePage + 1, max(offset, 0)}
	if e.d.offsetFirst {
		page[0], page[1] = page[1], page[0]
	}
	rows, err := e.db.QueryContext(ctx, q, append(args, page...)...)
	if err != nil {
		return nil, false, e.wrap(err)
	}
	defer rows.Close()
	nodes := []Node{}
	for rows.Next() {
		var n Node
		var detail sql.NullString // Oracle's '' is NULL
		if err := rows.Scan(&n.Kind, &n.Name, &detail); err != nil {
			return nil, false, e.wrap(err)
		}
		n.Detail = detail.String
		n.Path = append(slices.Clone(path), n.Name)
		n.HasChildren = n.Kind != "column"
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, false, e.wrap(err)
	}
	more := len(nodes) > TreePage
	return nodes[:min(len(nodes), TreePage)], more, nil
}

func (e *sqlEngine) Query(ctx context.Context, text string) (Results, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active != nil { // one request at a time on the session
		e.active.Close()
		if e.active.killed {
			e.dropSession()
		}
		e.active = nil
	}
	// Closing results that are not done cancels them, so a newer request
	// never waits for the rest of a long result. That ends the session;
	// the next request opens another.
	ctx, cancel := context.WithCancel(ctx)
	var rows *sql.Rows
	var err error
	var stopKill func() bool
	for try := 0; try < 2; try++ {
		if e.session == nil {
			if e.session, err = e.db.Conn(ctx); err != nil {
				e.session = nil
				cancel()
				return nil, e.wrap(err)
			}
			e.kill = nil
			if e.d.killer != nil {
				if e.kill, err = e.d.killer(ctx, e.db, e.session); err != nil {
					e.session.Close()
					e.session = nil
					cancel()
					return nil, e.wrap(err)
				}
			}
		}
		if e.kill != nil {
			// A cancel, or closing results not yet done, stops the
			// statement on the server too; results read to the end never
			// do. The statement may not answer before it ends, so this is
			// armed before it is sent.
			stopKill = context.AfterFunc(ctx, e.kill)
		}
		rows, err = e.session.QueryContext(ctx, text)
		if err != nil && stopKill != nil && !stopKill() {
			e.dropSession()
		}
		// A session that died (a cancel, a dropped lease) refuses before
		// sending anything, so the request runs once on a new one.
		if err == nil || !errors.Is(err, driver.ErrBadConn) && !errors.Is(err, sql.ErrConnDone) {
			break
		}
		if e.session != nil {
			e.session.Close()
			e.session = nil
		}
	}
	if err != nil {
		cancel()
		return nil, e.wrap(err)
	}
	r := &sqlResults{e: e, rows: rows, cancel: cancel, rerun: rerunnable(text, e.d), stopKill: stopKill}
	r.load()
	e.active = r
	return r, nil
}

// dropSession closes the session's connection instead of returning it to
// the pool, so a kill sent for its statement cannot reach another; e.mu is
// held.
func (e *sqlEngine) dropSession() {
	_ = e.session.Raw(func(any) error { return driver.ErrBadConn })
	e.session.Close()
	e.session = nil
}

func (e *sqlEngine) Close() error {
	e.mu.Lock()
	if e.active != nil {
		e.active.Close()
	}
	if e.session != nil {
		e.session.Close()
	}
	e.mu.Unlock()
	err := e.db.Close()
	if e.onClose != nil {
		e.onClose()
	}
	return err
}

// sqlResults reads a request's result sets from database/sql.
type sqlResults struct {
	e      *sqlEngine
	rows   *sql.Rows
	cancel context.CancelFunc
	done   bool // every result set was read
	cols   []Column
	vals   []any
	ptrs   []any
	rerun  bool
	once   sync.Once
	// stopKill keeps a finished request's cancel from reaching the server.
	stopKill func() bool
	killed   bool
}

// load reads the current result set's columns.
func (r *sqlResults) load() {
	r.cols = []Column{}
	types, _ := r.rows.ColumnTypes()
	for _, t := range types {
		dbType := t.DatabaseTypeName()
		r.cols = append(r.cols, Column{Name: t.Name(), Type: strings.ToLower(dbType), Binary: r.e.d.binary(dbType)})
	}
	r.vals = make([]any, len(r.cols))
	r.ptrs = make([]any, len(r.cols))
	for i := range r.vals {
		r.ptrs[i] = &r.vals[i]
	}
}

func (r *sqlResults) Columns() []Column { return r.cols }

func (r *sqlResults) Next() ([]any, bool, error) {
	if !r.rows.Next() {
		return nil, false, r.e.wrap(r.rows.Err())
	}
	if err := r.rows.Scan(r.ptrs...); err != nil {
		return nil, false, r.e.wrap(err)
	}
	return slices.Clone(r.vals), true, nil
}

func (r *sqlResults) NextResultSet() bool {
	if !r.rows.NextResultSet() {
		r.done = true
		return false
	}
	r.load()
	return true
}

func (r *sqlResults) Rerunnable() bool { return r.rerun }

func (r *sqlResults) Close() error {
	var err error
	r.once.Do(func() {
		if r.stopKill != nil {
			// killed: the kill was or will be sent, so the session must not
			// serve another statement it might reach.
			r.killed = !r.done || !r.stopKill()
		}
		if !r.done {
			r.cancel()
		}
		err = r.rows.Close()
		r.cancel()
	})
	return err
}

// readOnlyLead are the statements that only read; readOnlyAny never appear
// in one that only reads.
var (
	readOnlyLead = []string{"SELECT", "VALUES", "TABLE", "SHOW", "WITH", "EXPLAIN"}
	writeWords   = []string{"INSERT", "UPDATE", "DELETE", "MERGE", "INTO", "ANALYZE", "CALL", "COPY", "LOCK"}
)

// rerunnable judges by its text whether a request changes nothing: every
// statement starts with a reading keyword and no writing keyword appears
// outside quotes and comments. Anything the lexer cannot follow is not.
func rerunnable(text string, d dialect) bool {
	stmts, ok := statements(text, d)
	if !ok || len(stmts) == 0 {
		return false
	}
	for _, words := range stmts {
		if !slices.Contains(readOnlyLead, words[0]) {
			return false
		}
		for _, w := range words {
			if slices.Contains(writeWords, w) {
				return false
			}
		}
	}
	return true
}

// statements splits text at semicolons into each statement's words,
// upper-cased, skipping comments, string literals and quoted identifiers.
// ok is false when text ends inside a quote or comment.
func statements(text string, d dialect) (stmts [][]string, ok bool) {
	var cur []string
	end := func() {
		if len(cur) > 0 {
			stmts = append(stmts, cur)
		}
		cur = nil
	}
	isWord := func(c byte) bool {
		return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
	}
	// skipQuoted skips past the closing q of a literal opened at i, where
	// q doubled is an escaped q; backslash escapes too when esc.
	skipQuoted := func(i int, q byte, esc bool) int {
		for j := i + 1; j < len(text); j++ {
			switch {
			case esc && text[j] == '\\':
				j++
			case text[j] == q && j+1 < len(text) && text[j+1] == q:
				j++
			case text[j] == q:
				return j + 1
			}
		}
		return -1
	}
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == ';':
			end()
			i++
		case c == '-' && strings.HasPrefix(text[i:], "--") || c == '#' && d.hashComments:
			nl := strings.IndexByte(text[i:], '\n')
			if nl < 0 {
				i = len(text)
			} else {
				i += nl + 1
			}
		case c == '/' && strings.HasPrefix(text[i:], "/*"):
			depth, j := 1, i+2
			for depth > 0 && j < len(text) {
				switch {
				case strings.HasPrefix(text[j:], "*/"):
					depth, j = depth-1, j+2
				case d.nestedComments && strings.HasPrefix(text[j:], "/*"):
					depth, j = depth+1, j+2
				default:
					j++
				}
			}
			if depth > 0 {
				return nil, false
			}
			i = j
		case c == '\'' || c == '"' || c == '`' || c == '[' && !d.dollarQuotes:
			q := c
			if q == '[' {
				q = ']'
			}
			esc := c == '\'' && i > 0 && (text[i-1] == 'E' || text[i-1] == 'e') && (i == 1 || !isWord(text[i-2])) ||
				d.backslashEscapes && (c == '\'' || c == '"')
			if i = skipQuoted(i, q, esc); i < 0 {
				return nil, false
			}
		case c == '$' && d.dollarQuotes && i+1 < len(text) && !(text[i+1] >= '0' && text[i+1] <= '9'):
			j := i + 1
			for j < len(text) && isWord(text[j]) {
				j++
			}
			if j >= len(text) || text[j] != '$' {
				i = j
				break
			}
			tag := text[i : j+1]
			k := strings.Index(text[j+1:], tag)
			if k < 0 {
				return nil, false
			}
			i = j + 1 + k + len(tag)
		case isWord(c):
			j := i
			for j < len(text) && (isWord(text[j]) || text[j] == '$') {
				j++
			}
			if !(c >= '0' && c <= '9') {
				cur = append(cur, strings.ToUpper(text[i:j]))
			}
			i = j
		default:
			i++
		}
	}
	end()
	return stmts, true
}
