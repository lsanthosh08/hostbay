package database

import (
	"context"
	"net"
	"testing"

	"hostbay/internal/database/dbtest"
)

const myPassword = "my-Secret-5!"

func fakeMySQL(t *testing.T) (*dbtest.MySQL, ServerConfig) {
	srv := dbtest.NewMySQL(t)
	return srv, ServerConfig{Host: srv.Host, Port: srv.Port, User: "app", Password: myPassword, Database: "app", TLS: "none",
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}
}

// A server that asks for a local file gets an empty answer, never the
// file, and the query fails (KTD26).
func TestMySQLSendsNoLocalFile(t *testing.T) {
	srv, cfg := fakeMySQL(t)
	srv.AskFile("/etc/passwd")
	e, err := OpenMySQL(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.Query(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("the query asking for a local file succeeded")
	}
	if after := srv.After(); len(after) != 1 || len(after[0]) != 0 {
		t.Fatalf("the client answered the file request with %q", after)
	}
}

// A server that switches the sign-in to the cleartext password plugin is
// refused, and the password is never sent.
func TestMySQLRefusesTheCleartextPasswordPlugin(t *testing.T) {
	srv, cfg := fakeMySQL(t)
	srv.AskCleartext()
	if _, err := OpenMySQL(context.Background(), cfg); err == nil {
		t.Fatal("signed in over the cleartext password plugin")
	}
	if after := srv.After(); len(after) != 0 {
		t.Fatalf("the client answered the cleartext ask with %q", after)
	}
}

// MySQL strings take backslash escapes and # starts a comment, so neither
// can hide a write from the re-run check.
func TestMySQLRerunSeesBackslashesAndHashComments(t *testing.T) {
	for text, want := range map[string]bool{
		`SELECT 'it\'s'`:                         true,
		"SELECT 1 # '\nDELETE FROM t; -- '":      false,
		`SELECT 'x\''; DELETE FROM t; SELECT ''`: false,
		"SELECT `a``b` FROM t # DELETE":          true,
		`SELECT "say \"DELETE\"" FROM t`:         true,
	} {
		if got := rerunnable(text, mysqlDialect); got != want {
			t.Errorf("rerunnable(%q) = %v, want %v", text, got, want)
		}
	}
	if got := mysqlQuote("a`b"); got != "`a``b`" {
		t.Fatalf("quote = %s", got)
	}
}
