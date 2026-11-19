package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"slices"
	"sync"
	"testing"
)

// guardDriver is a database/sql driver that records every statement sent
// and answers each query with no rows.
type guardDriver struct {
	mu   sync.Mutex
	sent []string
}

func (d *guardDriver) Open(string) (driver.Conn, error) { return &guardConn{d}, nil }

func (d *guardDriver) record(q string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sent = append(d.sent, q)
}

func (d *guardDriver) statements() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.sent)
}

type guardConn struct{ d *guardDriver }

func (c *guardConn) Prepare(q string) (driver.Stmt, error) { return &guardStmt{c.d, q}, nil }
func (c *guardConn) Close() error                          { return nil }
func (c *guardConn) Begin() (driver.Tx, error)             { return c, nil }
func (c *guardConn) Commit() error                         { return nil }
func (c *guardConn) Rollback() error                       { return nil }

type guardStmt struct {
	d *guardDriver
	q string
}

func (s *guardStmt) Close() error  { return nil }
func (s *guardStmt) NumInput() int { return -1 }
func (s *guardStmt) Exec([]driver.Value) (driver.Result, error) {
	s.d.record(s.q)
	return driver.RowsAffected(0), nil
}
func (s *guardStmt) Query([]driver.Value) (driver.Rows, error) {
	s.d.record(s.q)
	return guardRows{}, nil
}

type guardRows struct{}

func (guardRows) Columns() []string         { return []string{"x"} }
func (guardRows) Close() error              { return nil }
func (guardRows) Next([]driver.Value) error { return io.EOF }

// In read-only mode the session is made read-only again before every
// request, not only when it opens: a request judged read-only by its text
// can still change the session's default (a settings function called from
// a SELECT), and the server guard must hold for the next one.
func TestReadOnlyGuardBeforeEveryRequest(t *testing.T) {
	for _, d := range []dialect{pgDialect, mysqlDialect, sqliteDialect} {
		d.killer = nil // MySQL's asks the server for its connection ID
		drv := &guardDriver{}
		name := "guard-" + d.readOnly(true)
		sql.Register(name, drv)
		db, err := sql.Open(name, "")
		if err != nil {
			t.Fatal(err)
		}
		e := newSQLEngine(db, d, nil)
		ctx := context.Background()
		if err := e.SetReadOnly(ctx, true); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"SELECT 1", "SELECT 2", "SELECT 3"} {
			r, err := e.Query(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
		}
		guard := d.readOnly(true)
		want := []string{guard, "SELECT 1", guard, "SELECT 2", guard, "SELECT 3"}
		if got := drv.statements(); !slices.Equal(got, want) {
			t.Errorf("%s: sent %q, want %q", guard, got, want)
		}
		if err := e.SetReadOnly(ctx, false); err != nil {
			t.Fatal(err)
		}
		before := len(drv.statements())
		r, _ := e.Query(ctx, "SELECT 4")
		r.Close()
		if got := drv.statements()[before:]; !slices.Equal(got, []string{d.readOnly(false), "SELECT 4"}) && !slices.Equal(got, []string{"SELECT 4"}) {
			t.Errorf("%s off: sent %q", guard, got)
		}
		e.Close()
	}
}
