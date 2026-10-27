package database

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/lib/pq"
)

// Dial opens a connection to address, which the dial function refuses
// unless it is the host's own (KTD24).
type Dial func(ctx context.Context, network, address string) (net.Conn, error)

// PGConfig is a PostgreSQL host, from its structured fields only (KTD26).
type PGConfig struct {
	Host, User, Password, Database string
	Port                           int
	TLS                            string // "verify" or "none" (KTD25)
	Dial                           Dial
	// Trust finds the TLS config as tlstrust.Capture does: try connects
	// and signs in; it never runs unverified.
	Trust func(try func(*tls.Config) error) (*tls.Config, error)
	// NoClientCert is a path that never exists. lib/pq reads a client
	// certificate and key from the home folder unless pointed elsewhere.
	NoClientCert string
}

// pgDialer hands lib/pq the dial function, for pool redials and cancel
// requests too.
type pgDialer struct{ dial Dial }

func (d pgDialer) Dial(network, address string) (net.Conn, error) {
	return d.dial(context.Background(), network, address)
}

func (d pgDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return d.dial(ctx, network, address)
}

func (d pgDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dial(ctx, network, address)
}

// OpenPostgres signs in to a PostgreSQL host. lib/pq is configured from
// fields, never a connection string, so a database name such as
// "x sslmode=disable" stays a literal name, and no PG* variable, service
// file or ~/.pgpass changes anything. Under "verify" the server's
// certificate is checked (or the prompt raised) before the startup
// message, which carries the password, is sent.
func OpenPostgres(ctx context.Context, c PGConfig) (Engine, error) {
	// PQGO_DEBUG traces the whole protocol, password message included, to
	// stderr; lib/pq reads it once at start, so it cannot be turned off.
	if os.Getenv("PQGO_DEBUG") != "" {
		return nil, errors.New("PQGO_DEBUG is set in Hostbay's environment, which would write your password to its output; quit Hostbay, unset it and start Hostbay again")
	}
	if c.Password == "" {
		return nil, errors.New("no password to sign in with")
	}
	for what, v := range map[string]string{"user name": c.User, "database name": c.Database} {
		if strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("the %s contains a control character", what)
		}
	}
	if c.Port < 1 || c.Port > 65535 {
		return nil, errors.New("the port must be between 1 and 65535")
	}
	cfg := pq.Config{
		Host: c.Host, Port: uint16(c.Port), User: c.User, Password: c.Password, Database: c.Database,
		ApplicationName: "Hostbay", SSLSNI: true,
		SSLCert: c.NoClientCert, SSLKey: c.NoClientCert,
		// "system" keeps lib/pq from reading ~/.postgresql/root.crt; the
		// roots and pins come from Trust's config, which checks in
		// VerifyConnection whatever RootCAs lib/pq sets.
		SSLRootCert: "system",
		SSLMode:     pq.SSLModeDisable,
	}
	var key string
	if c.TLS != "none" {
		key = "hostbay-" + newID()
		cfg.SSLMode = pq.SSLMode("pqgo-" + key)
	}
	connector, err := pq.NewConnectorConfig(cfg)
	if err != nil {
		return nil, err
	}
	connector.Dialer(pgDialer{c.Dial})
	unregister := func() {
		if key != "" {
			_ = pq.RegisterTLSConfig(key, nil)
		}
	}
	try := func() error {
		conn, err := connector.Connect(ctx)
		if err != nil {
			return err
		}
		return conn.Close()
	}
	if key == "" {
		err = try()
	} else {
		// The registry is global, so each connection has its own key,
		// removed when it closes.
		var final *tls.Config
		final, err = c.Trust(func(cfg *tls.Config) error {
			_ = pq.RegisterTLSConfig(key, cfg)
			return try()
		})
		if err == nil {
			_ = pq.RegisterTLSConfig(key, final)
		}
	}
	if err != nil {
		unregister()
		return nil, categorized(err, pgCategory)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(8)
	return newSQLEngine(db, pgDialect, unregister), nil
}

func pgCategory(err error) string {
	var pe *pq.Error
	if !errors.As(err, &pe) {
		return ""
	}
	code := string(pe.Code)
	switch {
	case code == "57014":
		return "cancelled"
	case strings.HasPrefix(code, "28"):
		return "auth"
	case code == "42501":
		return "permission"
	case strings.HasPrefix(code, "42"):
		return "syntax"
	case strings.HasPrefix(code, "08"):
		return "network"
	}
	return ""
}

func pgQuote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// pgDialect: schemas at the root, then their tables and views, then
// columns. System schemas are left out.
var pgDialect = dialect{
	quote:       pgQuote,
	placeholder: func(n int) string { return "$" + strconv.Itoa(n) },
	children: func(path []string) (string, []any) {
		switch len(path) {
		case 0:
			return `SELECT 'schema', nspname, '' FROM pg_catalog.pg_namespace
				WHERE nspname NOT LIKE 'pg\_%' AND nspname <> 'information_schema'
				ORDER BY nspname LIMIT $1 OFFSET $2`, nil
		case 1:
			return `SELECT CASE WHEN c.relkind IN ('v', 'm') THEN 'view' ELSE 'table' END, c.relname, ''
				FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
				ORDER BY c.relname LIMIT $2 OFFSET $3`, []any{path[0]}
		case 2:
			return `SELECT 'column', a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)
				FROM pg_catalog.pg_attribute a
				JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
				JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
				ORDER BY a.attnum LIMIT $3 OFFSET $4`, []any{path[0], path[1]}
		}
		return "", nil
	},
	key:          pgKey,
	tablePath:    func(path []string) bool { return len(path) == 2 },
	binary:       func(t string) bool { return strings.EqualFold(t, "BYTEA") },
	category:     pgCategory,
	dollarQuotes: true, nestedComments: true,
}

// pgKey is the table's primary key, else a unique index without a
// predicate or expressions over NOT NULL columns.
func pgKey(ctx context.Context, db *sql.DB, path []string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT i.indexrelid::text, a.attname, a.attnotnull
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class c ON c.oid = i.indrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE n.nspname = $1 AND c.relname = $2 AND (i.indisprimary OR i.indisunique)
			AND i.indpred IS NULL AND i.indexprs IS NULL
		ORDER BY i.indisprimary DESC, i.indexrelid, k.ord`, path[0], path[1])
	return firstKey(rows, err)
}

// firstKey reads a key query's rows: index, column and whether the column
// is NOT NULL, in key order with the primary key first. It returns the
// first index whose columns are all NOT NULL, or nil. A NULL column is an
// expression (a MySQL functional index), so its index is not a key.
func firstKey(rows *sql.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var order []string
	keys := map[string][]string{}
	usable := map[string]bool{}
	for rows.Next() {
		var ix string
		var col sql.NullString
		var notNull bool
		if err := rows.Scan(&ix, &col, &notNull); err != nil {
			return nil, err
		}
		if _, seen := keys[ix]; !seen {
			order = append(order, ix)
			usable[ix] = true
		}
		keys[ix] = append(keys[ix], col.String)
		usable[ix] = usable[ix] && notNull && col.Valid
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, ix := range order {
		if usable[ix] {
			return keys[ix], nil
		}
	}
	return nil, nil
}
