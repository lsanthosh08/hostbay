package database

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// OpenMySQL signs in to a MySQL or MariaDB host. The driver is configured
// from its config struct, never a DSN, with every weakening option fixed
// off (KTD26): no local files for LOAD DATA LOCAL INFILE, no cleartext or
// old-password sign-in, no fallback from TLS to plain text, and no driver
// log. Under "verify" the server's certificate is checked (or the prompt
// raised) before the handshake response, which carries the password.
func OpenMySQL(ctx context.Context, c ServerConfig) (Engine, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.DBName = c.User, c.Password, c.Database
	cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	cfg.DialFunc = c.Dial
	cfg.Logger = log.New(io.Discard, "", 0)
	cfg.Timeout = 15 * time.Second
	cfg.AllowAllFiles, cfg.AllowCleartextPasswords, cfg.AllowOldPasswords, cfg.AllowFallbackToPlaintext = false, false, false, false
	cfg.InterpolateParams, cfg.ParseTime = false, false
	// The editor sends its text as one request, as psql would run it.
	cfg.MultiStatements = true
	// Matched rows, not changed ones: saving a cell with its current value
	// matches one row and commits (KTD23).
	cfg.ClientFoundRows = true
	open := func(tc *tls.Config) (*sql.DB, error) {
		cfg.TLS = tc
		connector, err := mysql.NewConnector(cfg)
		if err != nil {
			return nil, err
		}
		return sql.OpenDB(connector), nil
	}
	try := func(tc *tls.Config) error {
		db, err := open(tc)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.PingContext(ctx)
	}
	var final *tls.Config
	var err error
	if c.TLS == "none" {
		err = try(nil)
	} else {
		final, err = c.Trust(try)
	}
	if err != nil {
		return nil, categorized(err, mysqlCategory)
	}
	db, err := open(final)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	return newSQLEngine(db, mysqlDialect, nil), nil
}

func mysqlCategory(err error) string {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return ""
	}
	switch me.Number {
	case 1317: // ER_QUERY_INTERRUPTED
		return "cancelled"
	case 1045, 1698: // access denied
		return "auth"
	case 1044, 1142, 1143, 1227, 1370:
		return "permission"
	case 1054, 1064, 1146, 1149:
		return "syntax"
	}
	return ""
}

func mysqlQuote(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

// mysqlDialect: databases at the root, then their tables and views, then
// columns. The system databases are left out.
var mysqlDialect = dialect{
	quote:       mysqlQuote,
	placeholder: func(int) string { return "?" },
	children: func(path []string) (string, []any) {
		switch len(path) {
		case 0:
			return `SELECT 'schema', SCHEMA_NAME, '' FROM information_schema.SCHEMATA
				WHERE SCHEMA_NAME NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
				ORDER BY SCHEMA_NAME LIMIT ? OFFSET ?`, nil
		case 1:
			return `SELECT CASE WHEN TABLE_TYPE = 'VIEW' THEN 'view' ELSE 'table' END, TABLE_NAME, ''
				FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?
				ORDER BY TABLE_NAME LIMIT ? OFFSET ?`, []any{path[0]}
		case 2:
			return `SELECT 'column', COLUMN_NAME, COLUMN_TYPE FROM information_schema.COLUMNS
				WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
				ORDER BY ORDINAL_POSITION LIMIT ? OFFSET ?`, []any{path[0], path[1]}
		}
		return "", nil
	},
	key: func(ctx context.Context, db *sql.DB, path []string) ([]string, error) {
		return firstKey(db.QueryContext(ctx, `SELECT INDEX_NAME, COLUMN_NAME, NULLABLE <> 'YES'
			FROM information_schema.STATISTICS
			WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND NON_UNIQUE = 0
			ORDER BY INDEX_NAME = 'PRIMARY' DESC, INDEX_NAME, SEQ_IN_INDEX`, path[0], path[1]))
	},
	tablePath: func(path []string) bool { return len(path) == 2 },
	binary: func(t string) bool {
		t = strings.ToUpper(t)
		return strings.HasSuffix(t, "BLOB") || t == "BINARY" || t == "VARBINARY" || t == "BIT" || t == "GEOMETRY"
	},
	category:         mysqlCategory,
	backslashEscapes: true, hashComments: true,
	// The driver cancels by dropping its connection, which leaves the
	// statement running on the server; KILL QUERY on another connection of
	// the pool, through the same dial function, ends it.
	killer: func(ctx context.Context, db *sql.DB, session *sql.Conn) (func(), error) {
		var id uint64
		if err := session.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&id); err != nil {
			return nil, err
		}
		return func() {
			ctx, cancel := context.WithTimeout(context.Background(), CatalogTimeout)
			defer cancel()
			_, _ = db.ExecContext(ctx, "KILL QUERY "+strconv.FormatUint(id, 10))
		}, nil
	},
}
