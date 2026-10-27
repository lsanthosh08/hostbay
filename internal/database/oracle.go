package database

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	go_ora "github.com/sijms/go-ora/v2"
	"github.com/sijms/go-ora/v2/configurations"
	"github.com/sijms/go-ora/v2/network"
)

// oraConnector opens each connection from its own copy of the config:
// go-ora writes to the TLS config and server list it is given. Its config
// struct is filled from fields, never a connection string (KTD26).
type oraConnector struct {
	cfg  configurations.ConnectionConfig
	dial Dial
}

func (c oraConnector) Connect(ctx context.Context) (driver.Conn, error) {
	cfg := c.cfg
	cfg.Servers = slices.Clone(cfg.Servers)
	if cfg.TLSConfig != nil {
		cfg.TLSConfig = cfg.TLSConfig.Clone()
	}
	// One dial per connection: a listener redirect, even to the same
	// address over plain TCP, would leave the checked connection.
	var mu sync.Mutex
	dials := 0
	cfg.RegisterDial(func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dials++
		n := dials
		mu.Unlock()
		if n > 1 {
			return nil, fmt.Errorf("the Oracle listener redirected the connection to %s, and Hostbay connects only to the address it was given: point the host at the database's own listener address", addr)
		}
		return c.dial(ctx, network, addr)
	})
	conn, err := go_ora.NewConnection("", &cfg)
	if err != nil {
		return nil, err
	}
	if err := conn.OpenWithContext(ctx); err != nil {
		return nil, err
	}
	return conn, nil
}

func (c oraConnector) Driver() driver.Driver { return go_ora.NewDriver() }

// OpenOracle signs in to an Oracle service (Database is its name). Under
// "verify" it connects over TCPS, the certificate checked (or the prompt
// raised) before the connect packet; native network encryption without
// TCPS does not authenticate the server, so it counts as "none" (KTD25).
// Tracing is off.
func OpenOracle(ctx context.Context, c ServerConfig) (Engine, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if c.User == "" {
		return nil, errors.New("enter the user name to sign in with")
	}
	// The service name goes into the connect descriptor as it is.
	if c.Database == "" || strings.ContainsAny(c.Database, "()=") {
		return nil, errors.New("enter the Oracle service name, without parentheses or =")
	}
	// A fixed address and nothing from the host: ParseConfig only supplies
	// go-ora's defaults, and every field that matters is set below.
	cfg, err := go_ora.ParseConfig("oracle://hostbay:unused@hostbay.invalid:1521/unused")
	if err != nil {
		return nil, err
	}
	proto := "tcps"
	if c.TLS == "none" {
		proto = "tcp"
	}
	cfg.UserID, cfg.Password, cfg.ServiceName, cfg.SID = c.User, c.Password, c.Database, ""
	cfg.DBAPrivilege = configurations.NONE
	if strings.EqualFold(c.User, "SYS") {
		cfg.DBAPrivilege = configurations.SYSDBA
	}
	cfg.Servers = []configurations.ServerAddr{{Protocol: proto, Addr: c.Host, Port: c.Port}}
	cfg.Protocol, cfg.SSL, cfg.SSLVerify = proto, proto == "tcps", true
	cfg.AuthType, cfg.AuthService, cfg.Wallet = configurations.Normal, nil, nil
	cfg.TraceFilePath, cfg.TraceDir, cfg.UnixAddress = "", "", ""
	cfg.ProgramName = "Hostbay"
	// go-ora sets a deadline before every read and write, which an SSH
	// channel (a jump host's connection) refuses.
	conn := oraConnector{cfg: *cfg, dial: withDeadlines(c.Dial)}
	try := func(tc *tls.Config) error {
		conn.cfg.TLSConfig = tc
		dc, err := conn.Connect(ctx)
		if err != nil {
			return err
		}
		return dc.Close()
	}
	if c.TLS == "none" {
		err = try(nil)
	} else {
		var final *tls.Config
		final, err = c.Trust(try)
		conn.cfg.TLSConfig = final
	}
	if err != nil {
		return nil, categorized(err, oraCategory)
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(8)
	return newSQLEngine(db, oraDialect, nil), nil
}

func oraCategory(err error) string {
	var oe *network.OracleError
	if !errors.As(err, &oe) {
		return ""
	}
	switch c := oe.ErrCode; {
	case c == 1013: // user requested cancel
		return "cancelled"
	case c == 1017 || c == 28000 || c == 1005:
		return "auth"
	case c == 1031 || c == 1039 || c == 1045:
		return "permission"
	case c >= 900 && c <= 999:
		return "syntax"
	}
	return ""
}

func oraQuote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// oraDialect: schemas (users that are not Oracle's own) at the root, then
// their tables and views, then columns.
var oraDialect = dialect{
	quote:       oraQuote,
	placeholder: func(n int) string { return ":" + strconv.Itoa(n) },
	children: func(path []string) (string, []any) {
		switch len(path) {
		case 0:
			return `SELECT 'schema', username, '' FROM all_users WHERE oracle_maintained = 'N'
				ORDER BY username OFFSET :1 ROWS FETCH NEXT :2 ROWS ONLY`, nil
		case 1:
			return `SELECT kind, name, '' FROM (
					SELECT 'table' kind, table_name name FROM all_tables WHERE owner = :1
					UNION ALL SELECT 'view', view_name FROM all_views WHERE owner = :2)
				ORDER BY name OFFSET :3 ROWS FETCH NEXT :4 ROWS ONLY`, []any{path[0], path[0]}
		case 2:
			return `SELECT 'column', column_name, data_type FROM all_tab_columns
				WHERE owner = :1 AND table_name = :2
				ORDER BY column_id OFFSET :3 ROWS FETCH NEXT :4 ROWS ONLY`, []any{path[0], path[1]}
		}
		return "", nil
	},
	key: func(ctx context.Context, db *sql.DB, path []string) ([]string, error) {
		return firstKey(db.QueryContext(ctx, `SELECT c.constraint_name, cc.column_name,
				CASE WHEN tc.nullable = 'N' THEN 1 ELSE 0 END
			FROM all_constraints c
			JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
			JOIN all_tab_columns tc ON tc.owner = c.owner AND tc.table_name = c.table_name AND tc.column_name = cc.column_name
			WHERE c.owner = :1 AND c.table_name = :2 AND c.constraint_type IN ('P', 'U')
			ORDER BY CASE c.constraint_type WHEN 'P' THEN 0 ELSE 1 END, c.constraint_name, cc.position`, path[0], path[1]))
	},
	tablePath: func(path []string) bool { return len(path) == 2 },
	binary: func(t string) bool {
		switch strings.ToUpper(t) {
		case "RAW", "LONG RAW", "LONGRAW", "BLOB":
			return true
		}
		return false
	},
	category: oraCategory,
	// OFFSET :n ROWS FETCH NEXT :m binds by position: the offset first.
	offsetFirst: true,
}
