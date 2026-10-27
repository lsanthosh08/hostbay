package database

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"hostbay/internal/transfer"
)

// rqMaxBody bounds one rqlite answer: rqlite returns a statement's rows
// whole, never in pages.
const rqMaxBody = 64 << 20

// OpenRqlite connects to one rqlite node at c.URL over its HTTP API, on
// transfer's client: every connection through the dial function, no
// environment proxy, and nothing beyond the node's own origin, so neither
// a leader redirect nor node discovery leaves the jump host or proxy
// (KTD24, KTD30). rqlite is SQLite underneath, so it shares SQLite's
// catalog and keys over a small database/sql driver.
func OpenRqlite(ctx context.Context, c ServerConfig) (Engine, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("enter the rqlite node's URL, starting https:// (or http:// to connect unencrypted)")
	}
	origin := &url.URL{Scheme: u.Scheme, Host: u.Host}
	client := func(tc *tls.Config) *http.Client {
		cl := transfer.HTTPClient(tc, origin, transfer.DialFunc(c.Dial))
		cl.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
			return fmt.Errorf("the rqlite node sent the request on to %s, and Hostbay connects only to %s: point the host at the cluster's leader node", req.URL.Host, origin.Host)
		}
		return cl
	}
	conn := &rqConnector{base: origin.String(), user: c.User, password: c.Password}
	try := func(tc *tls.Config) error {
		conn.cl = client(tc)
		_, err := conn.do(ctx, "/db/request", "SELECT 1", nil)
		return err
	}
	if u.Scheme == "http" {
		err = try(nil)
	} else {
		var final *tls.Config
		if final, err = c.Trust(try); err == nil {
			conn.cl = client(final)
		}
	}
	if err != nil {
		return nil, categorized(err, Category)
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(8)
	return newSQLEngine(db, rqliteDialect, nil), nil
}

// rqliteDialect is SQLite's, with rqlite's errors.
var rqliteDialect = func() dialect {
	d := sqliteDialect
	d.category = func(err error) string {
		msg := err.Error()
		if strings.Contains(msg, "syntax error") || strings.Contains(msg, "no such ") {
			return "syntax"
		}
		return ""
	}
	return d
}()

// rqConnector is the database/sql connector of one rqlite node. Its
// connections are stateless HTTP requests.
type rqConnector struct {
	cl             *http.Client
	base           string
	user, password string
}

func (c *rqConnector) Connect(context.Context) (driver.Conn, error) { return rqConn{c}, nil }
func (c *rqConnector) Driver() driver.Driver                        { return rqDriver{} }

type rqDriver struct{}

func (rqDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("rqlite opens only through Hostbay's connector")
}

// rqResult is one statement's answer.
type rqResult struct {
	Columns      []string `json:"columns"`
	Types        []string `json:"types"`
	Values       [][]any  `json:"values"`
	RowsAffected int64    `json:"rows_affected"`
	Error        string   `json:"error"`
}

// do sends one statement with its parameters to path: /db/request runs a
// read or a write, /db/execute a write.
func (c *rqConnector) do(ctx context.Context, path, stmt string, args []driver.NamedValue) (rqResult, error) {
	s := []any{stmt}
	for _, a := range args {
		s = append(s, a.Value)
	}
	body, err := json.Marshal([][]any{s})
	if err != nil {
		return rqResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return rqResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	resp, err := c.cl.Do(req)
	if err != nil {
		return rqResult{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, rqMaxBody+1))
	switch {
	case err != nil:
		return rqResult{}, err
	case resp.StatusCode == http.StatusUnauthorized:
		return rqResult{}, &Error{Category: "auth", Err: errors.New("rqlite refused the user name or password")}
	case resp.StatusCode == http.StatusForbidden:
		return rqResult{}, &Error{Category: "permission", Err: errors.New("this rqlite user may not do that")}
	case resp.StatusCode != http.StatusOK:
		return rqResult{}, fmt.Errorf("rqlite answered %s: %s", resp.Status, bytes.TrimSpace(b[:min(len(b), 512)]))
	case len(b) > rqMaxBody:
		return rqResult{}, errors.New("rqlite's answer is over 64 MiB; add a LIMIT to the query")
	}
	var out struct {
		Results []rqResult `json:"results"`
		Error   string     `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return rqResult{}, fmt.Errorf("rqlite's answer is not JSON: %w", err)
	}
	switch {
	case out.Error != "":
		return rqResult{}, errors.New(out.Error)
	case len(out.Results) != 1:
		return rqResult{}, fmt.Errorf("rqlite answered %d results for one statement", len(out.Results))
	case out.Results[0].Error != "":
		return rqResult{}, errors.New(out.Results[0].Error)
	}
	return out.Results[0], nil
}

type rqConn struct{ c *rqConnector }

func (rqConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("rqlite takes no prepared statements")
}
func (rqConn) Close() error { return nil }

// Begin returns a transaction that does nothing.
// shortcut: rqlite has no interactive transactions, so a row edit's
// rollback cannot undo; its UPDATE names a unique key, so it changes at
// most one row anyway (KTD23). Upgrade if rqlite gains them.
func (rqConn) Begin() (driver.Tx, error) { return rqTx{}, nil }

func (c rqConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	r, err := c.c.do(ctx, "/db/request", q, args)
	if err != nil {
		return nil, err
	}
	return &rqRows{r: r}, nil
}

func (c rqConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	r, err := c.c.do(ctx, "/db/execute", q, args)
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(r.RowsAffected), nil
}

type rqTx struct{}

func (rqTx) Commit() error   { return nil }
func (rqTx) Rollback() error { return nil }

type rqRows struct {
	r rqResult
	i int
}

func (r *rqRows) Columns() []string {
	if r.r.Columns == nil {
		return []string{}
	}
	return r.r.Columns
}

func (r *rqRows) Close() error { return nil }

func (r *rqRows) ColumnTypeDatabaseTypeName(i int) string {
	if i < len(r.r.Types) {
		return strings.ToUpper(r.r.Types[i])
	}
	return ""
}

// Next gives numbers as their exact text and blobs, which rqlite sends as
// base64, as bytes.
func (r *rqRows) Next(dest []driver.Value) error {
	if r.i >= len(r.r.Values) {
		return io.EOF
	}
	for j, v := range r.r.Values[r.i] {
		if j >= len(dest) {
			break
		}
		switch x := v.(type) {
		case nil, bool, string:
			dest[j] = x
			if s, ok := x.(string); ok && strings.EqualFold(r.ColumnTypeDatabaseTypeName(j), "BLOB") {
				if b, err := base64.StdEncoding.DecodeString(s); err == nil {
					dest[j] = b
				}
			}
		case json.Number:
			dest[j] = x.String()
		default:
			b, _ := json.Marshal(x)
			dest[j] = string(b)
		}
	}
	r.i++
	return nil
}
