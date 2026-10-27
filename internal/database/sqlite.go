package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

// SQLiteDSN is the one place a SQLite DSN is built (KTD26): "file:", the
// percent-escaped absolute path, and a fixed query that opens an existing
// file read-write without creating one and with trusted_schema off, so a
// view or trigger in the file cannot call unsafe functions. No other input
// reaches the query part.
func SQLiteDSN(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("the SQLite file must be an absolute path")
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") { // C:/x.db
		p = "/" + p
	}
	return "file://" + (&url.URL{Path: p}).EscapedPath() + "?mode=rw&_pragma=trusted_schema(0)", nil
}

// OpenSQLite opens a SQLite file on this computer. SQLite never dials. A
// missing file is reported, not created.
func OpenSQLite(ctx context.Context, path string) (Engine, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open the SQLite file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a file", path)
	}
	dsn, err := SQLiteDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("cannot open the SQLite file %s: %w", path, err)
	}
	return newSQLEngine(db, sqliteDialect, nil), nil
}

func sqliteQuote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// sqliteDialect: tables and views at the root, their columns below.
var sqliteDialect = dialect{
	quote:       sqliteQuote,
	placeholder: func(int) string { return "?" },
	children: func(path []string) (string, []any) {
		switch len(path) {
		case 0:
			return `SELECT type, name, '' FROM sqlite_schema WHERE type IN ('table', 'view') AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name LIMIT ? OFFSET ?`, nil
		case 1:
			return `SELECT 'column', name, type FROM pragma_table_info(?) ORDER BY cid LIMIT ? OFFSET ?`, []any{path[0]}
		}
		return "", nil
	},
	key:       sqliteKey,
	tablePath: func(path []string) bool { return len(path) == 1 },
	binary:    func(t string) bool { return strings.EqualFold(t, "BLOB") },
	category: func(err error) string {
		var se *sqlite.Error
		if !errors.As(err, &se) {
			return ""
		}
		switch se.Code() & 0xff {
		case 9: // SQLITE_INTERRUPT
			return "cancelled"
		case 3, 23: // SQLITE_PERM, SQLITE_AUTH
			return "permission"
		case 1: // SQLITE_ERROR: most often a syntax or unknown-name error
			return "syntax"
		}
		return ""
	},
}

// sqliteKey is the table's primary key, else a unique index over NOT NULL
// columns.
func sqliteKey(ctx context.Context, db *sql.DB, path []string) ([]string, error) {
	cols := func(q string, args ...any) ([]string, bool, error) {
		rows, err := db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, false, err
		}
		defer rows.Close()
		var out []string
		all := true
		for rows.Next() {
			var name sql.NullString
			var notNull bool
			if err := rows.Scan(&name, &notNull); err != nil {
				return nil, false, err
			}
			all = all && name.Valid && notNull
			out = append(out, name.String)
		}
		return out, all, rows.Err()
	}
	pk, _, err := cols(`SELECT name, 1 FROM pragma_table_info(?) WHERE pk > 0 ORDER BY pk`, path[0])
	if err != nil || len(pk) > 0 {
		return pk, err
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_index_list(?) WHERE "unique" = 1 AND partial = 0 ORDER BY seq`, path[0])
	if err != nil {
		return nil, err
	}
	var indexes []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		indexes = append(indexes, n)
	}
	rows.Close()
	for _, ix := range indexes {
		key, all, err := cols(`SELECT ii.name, coalesce(ti."notnull", 0) FROM pragma_index_info(?) ii LEFT JOIN pragma_table_info(?) ti ON ti.name = ii.name ORDER BY ii.seqno`, ix, path[0])
		if err != nil {
			return nil, err
		}
		if all && len(key) > 0 {
			return key, nil
		}
	}
	return nil, nil
}
