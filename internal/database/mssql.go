package database

import (
	"context"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
)

// mssqlDialer hands go-mssqldb the dial function. As a HostDialer it gets
// the host name, not an address this computer resolved, so DNS resolves
// beyond the jump host or proxy too. An address other than the host's own,
// where SQL Server routing sends the client, is refused (KTD24).
type mssqlDialer struct {
	dial Dial
	host string
	want string
}

func (d mssqlDialer) HostName() string { return d.host }

func (d mssqlDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if addr != d.want {
		return nil, fmt.Errorf("SQL Server sent the connection on to %s, and Hostbay connects only to %s: point the host at that server's own address, or switch Azure SQL to the Proxy connection policy", addr, d.want)
	}
	return d.dial(ctx, network, addr)
}

// mssqlConnector opens each connection from its own copy of the config:
// go-mssqldb writes to the TLS config it is given, so pool connections
// opening at once must not share one.
type mssqlConnector struct {
	cfg    msdsn.Config
	dialer mssqlDialer
}

func (c mssqlConnector) Connect(ctx context.Context) (driver.Conn, error) {
	cfg := c.cfg
	if cfg.TLSConfig != nil {
		cfg.TLSConfig = cfg.TLSConfig.Clone()
	}
	conn := mssql.NewConnectorConfig(cfg)
	conn.Dialer = c.dialer
	return conn.Connect(ctx)
}

func (c mssqlConnector) Driver() driver.Driver { return &mssql.Driver{} }

// OpenMSSQL signs in to a SQL Server host with a SQL Server login, from
// structured fields only (KTD26). Under "verify" the connection is TLS
// throughout, checked (or the prompt raised) before LOGIN7, which carries
// the password: TDS 8 strict encryption when the server speaks it, else
// encrypt=true with the same check (KTD25). "none" asks for no encryption.
func OpenMSSQL(ctx context.Context, c ServerConfig) (Engine, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	// No user would sign in as this computer's Windows user, and a
	// DOMAIN\user with NTLM: only SQL Server logins are offered.
	if c.User == "" || strings.Contains(c.User, `\`) {
		return nil, errors.New("sign in with a SQL Server login; Windows sign-in is not supported")
	}
	cfg := msdsn.Config{
		Host: c.Host, Port: uint64(c.Port), Database: c.Database, User: c.User, Password: c.Password,
		AppName: "Hostbay", Protocols: []string{"tcp"}, DialTimeout: 15 * time.Second,
		Encoding:   msdsn.EncodeParameters{Timezone: time.UTC},
		Encryption: msdsn.EncryptionDisabled,
	}
	want := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	conn := mssqlConnector{cfg: cfg, dialer: mssqlDialer{dial: c.Dial, host: c.Host, want: want}}
	try := func(enc msdsn.Encryption, tc *tls.Config) error {
		conn.cfg.Encryption, conn.cfg.TLSConfig = enc, tc
		dc, err := conn.Connect(ctx)
		if err != nil {
			return err
		}
		return dc.Close()
	}
	var err error
	if c.TLS == "none" {
		err = try(msdsn.EncryptionDisabled, nil)
	} else {
		var final *tls.Config
		strict := true
		final, err = c.Trust(func(tc *tls.Config) error {
			// A server that speaks TDS 8 answers the TLS handshake first;
			// one that never reached the certificate check does not, and
			// gets encrypt=true, checked the same way.
			reached := false
			first := tc.Clone()
			first.VerifyConnection = func(cs tls.ConnectionState) error {
				reached = true
				if tc.VerifyConnection == nil {
					return nil // crypto/tls's own check has run
				}
				return tc.VerifyConnection(cs)
			}
			err := try(msdsn.EncryptionStrict, first)
			if err == nil || reached {
				strict = true
				return err
			}
			strict = false
			return try(msdsn.EncryptionRequired, tc)
		})
		conn.cfg.Encryption, conn.cfg.TLSConfig = msdsn.EncryptionRequired, final
		if strict {
			conn.cfg.Encryption = msdsn.EncryptionStrict
		}
	}
	if err != nil {
		return nil, categorized(err, mssqlCategory)
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(8)
	return newSQLEngine(db, mssqlDialect, nil), nil
}

func mssqlCategory(err error) string {
	var me mssql.Error
	if !errors.As(err, &me) {
		return ""
	}
	switch me.Number {
	case 18452, 18456, 18486, 18487, 18488:
		return "auth"
	case 229, 230, 262, 297, 300, 916:
		return "permission"
	case 102, 105, 156, 170, 207, 208, 4145:
		return "syntax"
	case 3980:
		return "cancelled"
	}
	return ""
}

func mssqlQuote(name string) string { return "[" + strings.ReplaceAll(name, "]", "]]") + "]" }

// mssqlDialect: schemas at the root, then their tables and views, then
// columns. System and fixed-role schemas are left out. Placeholders are
// numbered, so OFFSET … FETCH names the limit and offset by number.
var mssqlDialect = dialect{
	quote:       mssqlQuote,
	placeholder: func(n int) string { return "@p" + strconv.Itoa(n) },
	children: func(path []string) (string, []any) {
		switch len(path) {
		case 0:
			return `SELECT 'schema', name, '' FROM sys.schemas
				WHERE schema_id < 16384 AND name NOT IN ('sys', 'INFORMATION_SCHEMA', 'guest')
				ORDER BY name OFFSET @p2 ROWS FETCH NEXT @p1 ROWS ONLY`, nil
		case 1:
			return `SELECT CASE o.type WHEN 'V' THEN 'view' ELSE 'table' END, o.name, ''
				FROM sys.objects o JOIN sys.schemas s ON s.schema_id = o.schema_id
				WHERE s.name = @p1 AND o.type IN ('U', 'V')
				ORDER BY o.name OFFSET @p3 ROWS FETCH NEXT @p2 ROWS ONLY`, []any{path[0]}
		case 2:
			return `SELECT 'column', c.name, TYPE_NAME(c.user_type_id)
				FROM sys.columns c
				JOIN sys.objects o ON o.object_id = c.object_id
				JOIN sys.schemas s ON s.schema_id = o.schema_id
				WHERE s.name = @p1 AND o.name = @p2
				ORDER BY c.column_id OFFSET @p4 ROWS FETCH NEXT @p3 ROWS ONLY`, []any{path[0], path[1]}
		}
		return "", nil
	},
	key: func(ctx context.Context, db *sql.DB, path []string) ([]string, error) {
		return firstKey(db.QueryContext(ctx, `SELECT CAST(i.index_id AS varchar(10)), c.name,
				CAST(CASE WHEN c.is_nullable = 0 THEN 1 ELSE 0 END AS bit)
			FROM sys.indexes i
			JOIN sys.objects o ON o.object_id = i.object_id
			JOIN sys.schemas s ON s.schema_id = o.schema_id
			JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
			JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
			WHERE s.name = @p1 AND o.name = @p2 AND (i.is_primary_key = 1 OR i.is_unique = 1)
				AND i.has_filter = 0 AND ic.is_included_column = 0
			ORDER BY i.is_primary_key DESC, i.index_id, ic.key_ordinal`, path[0], path[1]))
	},
	tablePath: func(path []string) bool { return len(path) == 2 },
	binary: func(t string) bool {
		switch strings.ToUpper(t) {
		case "BINARY", "VARBINARY", "IMAGE", "TIMESTAMP", "ROWVERSION":
			return true
		}
		return false
	},
	category:       mssqlCategory,
	nestedComments: true,
}
