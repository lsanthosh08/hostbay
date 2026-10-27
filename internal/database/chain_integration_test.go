//go:build integration

package database

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/dockertest"
	"hostbay/internal/sshx"
	"hostbay/internal/tlstrust"
)

// sshdScript makes a stock Alpine container an OpenSSH server that accepts
// $PUB for user u and forwards TCP, so it can be a hop.
const sshdScript = `apk add --no-cache openssh-server >/dev/null &&
ssh-keygen -A >/dev/null && adduser -D u && echo u:unused | chpasswd &&
mkdir -p /home/u/.ssh && echo "$PUB" > /home/u/.ssh/authorized_keys &&
chown -R u:u /home/u/.ssh && chmod 700 /home/u/.ssh && chmod 600 /home/u/.ssh/authorized_keys &&
touch /run/ready && exec /usr/sbin/sshd -D -e -o AllowTcpForwarding=yes -o PasswordAuthentication=no`

// twoHopChain starts chainClient's chain on a new network the database
// containers join, and returns the network and the client.
func twoHopChain(t *testing.T) (string, *sshx.Client) {
	t.Helper()
	network := dockertest.Network(t)
	return network, chainClient(t, network)
}

// pinThenOpen opens an engine in verify mode the way a user does: the
// first open raises the trust prompt, which is accepted, and the second
// connects under the pin. trust is wired into open's config.
func pinThenOpen(t *testing.T, certs *tlstrust.Store, open func() (Engine, error)) Engine {
	t.Helper()
	_, err := open()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown {
		t.Fatalf("first connect: %v", err)
	}
	if err := certs.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	e, err := open()
	if err != nil {
		t.Fatalf("connect under the pin: %v", err)
	}
	return e
}

// count is how many dials went through the chain so far.
func (d *chainDial) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

// all runs text and returns every row of its results.
func all(t *testing.T, r *Registry, e Engine, text string) [][]Cell {
	t.Helper()
	ctx := context.Background()
	id := r.Start(ctx, "tab", func(ctx context.Context) (Results, error) { return e.Query(ctx, text) })
	var rows [][]Cell
	for {
		p, err := r.Next(ctx, id)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		rows = append(rows, p.Rows...)
		if p.Done {
			return rows
		}
	}
}

// browseQueryPageEdit is what every SQL engine must do through the chain:
// list table (1200 rows, key column col, under schema when the engine has
// schemas), page through its rows, and edit one, also to its current value.
func browseQueryPageEdit(t *testing.T, e Engine, schema, table, col string) {
	t.Helper()
	ctx := context.Background()
	r := NewRegistry(func(State) {})
	var path []string
	if schema != "" {
		path = []string{schema}
		schemas, _, err := e.Children(ctx, nil, 0)
		if err != nil || !hasNode(schemas, schema) {
			t.Fatalf("schemas = %+v, %v", schemas, err)
		}
	}
	tables, _, err := e.Children(ctx, path, 0)
	if err != nil || !hasNode(tables, table) {
		t.Fatalf("tables = %+v, %v", tables, err)
	}
	path = append(path, table)
	cols, _, err := e.Children(ctx, path, 0)
	if err != nil || !hasNode(cols, col) {
		t.Fatalf("columns = %+v, %v", cols, err)
	}
	id := r.Start(ctx, "tab", func(ctx context.Context) (Results, error) {
		return e.Query(ctx, "SELECT "+col+" FROM "+table+" ORDER BY "+col)
	})
	var sizes []int
	for {
		p, err := r.Next(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Rows) > 0 {
			sizes = append(sizes, len(p.Rows))
		}
		if p.Done {
			break
		}
	}
	if fmt.Sprint(sizes) != "[500 500 200]" {
		t.Fatalf("pages = %v", sizes)
	}
	ed := e.(RowEditor)
	for _, kv := range [][2]string{{"1", "-1"}, {"-1", "-1"}} { // the second saves the current value
		key, set := kv[0], kv[1]
		if err := ed.EditRow(ctx, RowEdit{Path: path, Key: map[string]*string{col: &key}, Set: map[string]*string{col: &set}}); err != nil {
			t.Fatalf("edit %s to %s: %v", key, set, err)
		}
	}
	if rows := all(t, r, e, "SELECT count(*) FROM "+table+" WHERE "+col+" = -1"); rows[0][0].Text != "1" {
		t.Fatalf("edited rows = %+v", rows)
	}
}

// editQuoted edits the column named name, which the test created with
// that awkward name, of the row whose key column key is 1, and checks it
// changed.
func editQuoted(t *testing.T, e Engine, path []string, key, name, check string) {
	t.Helper()
	id, v := "1", "y"
	if err := e.(RowEditor).EditRow(context.Background(), RowEdit{Path: path, Key: map[string]*string{key: &id}, Set: map[string]*string{name: &v}}); err != nil {
		t.Fatalf("edit %q: %v", name, err)
	}
	if rows := all(t, NewRegistry(func(State) {}), e, check); rows[0][0].Text != "y" {
		t.Fatalf("%q = %+v", name, rows)
	}
}

func hasNode(nodes []Node, name string) bool {
	for _, n := range nodes {
		if n.Name == name {
			return true
		}
	}
	return false
}
