package database

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStatementsSkipQuotesAndComments(t *testing.T) {
	for _, c := range []struct {
		text string
		want [][]string
	}{
		{"select 1", [][]string{{"SELECT"}}},
		{"INSERT INTO t VALUES (1); SELECT * FROM t;", [][]string{{"INSERT", "INTO", "T", "VALUES"}, {"SELECT", "FROM", "T"}}},
		{"SELECT 'a;b DELETE', \"x;y\" -- ; UPDATE\n/* ; INSERT */ FROM t", [][]string{{"SELECT", "FROM", "T"}}},
		{"SELECT $$;DELETE$$, $tag$ ; $tag$, $1", [][]string{{"SELECT"}}},
		{"SELECT E'it\\'s;' ; SELECT 2", [][]string{{"SELECT", "E"}, {"SELECT"}}},
		{"/* outer /* inner */ still */ SELECT 1", [][]string{{"SELECT"}}},
	} {
		got, ok := statements(c.text, pgDialect)
		if !ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("statements(%q) = %v, %v; want %v", c.text, got, ok, c.want)
		}
	}
	for _, open := range []string{"SELECT 'x", "SELECT $$x", "/* x"} {
		if _, ok := statements(open, pgDialect); ok {
			t.Errorf("statements(%q) followed an unterminated quote", open)
		}
	}
}

func TestRerunnableOnlyForRequestsThatOnlyRead(t *testing.T) {
	for text, want := range map[string]bool{
		"SELECT * FROM t":                                true,
		"select 1; values (2); show search_path":         true,
		"INSERT INTO t VALUES (1); SELECT 1":             false,
		"WITH d AS (DELETE FROM t RETURNING *) SELECT 1": false,
		"SELECT * INTO copy FROM t":                      false,
		"EXPLAIN ANALYZE DELETE FROM t":                  false,
		"SELECT 'DELETE'":                                true,
		"UPDATE t SET v = 1":                             false,
		"":                                               false,
		"SELECT 'unterminated":                           false,
	} {
		if got := rerunnable(text, pgDialect); got != want {
			t.Errorf("rerunnable(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestEncodeShowsBinaryAsSizeAndHexAndTextAsText(t *testing.T) {
	c, full := encode([]byte{0xde, 0xad, 0xbe, 0xef}, true)
	if !c.Binary || c.Text != "deadbeef" || c.Size != 4 || len(full) != 4 {
		t.Fatalf("binary = %+v", c)
	}
	c, full = encode([]byte(`<img src=x onerror=alert(1)>`), false)
	if c.Text != `<img src=x onerror=alert(1)>` || c.Binary || full != nil {
		t.Fatalf("text = %+v", c)
	}
	if c, _ := encode(nil, false); !c.Null {
		t.Fatal("NULL is not marked")
	}
	long := strings.Repeat("é", CellCap) // two bytes each
	c, full = encode(long, false)
	if !c.Cut || len(c.Text) != CellCap || c.Size != len(long) || string(full) != long {
		t.Fatalf("long: cut %v, %d of %d", c.Cut, len(c.Text), c.Size)
	}
}

// fakeResults is one endless result set that counts closes.
type fakeResults struct{ closed chan struct{} }

func (f *fakeResults) Columns() []Column          { return []Column{{Name: "x"}} }
func (f *fakeResults) Next() ([]any, bool, error) { return []any{int64(1)}, true, nil }
func (f *fakeResults) NextResultSet() bool        { return false }
func (f *fakeResults) Rerunnable() bool           { return true }
func (f *fakeResults) Close() error               { close(f.closed); return nil }

// A tab holds at most PerOwner cursors: a further request closes the
// oldest, and closing the tab closes the rest.
func TestRegistryBoundsCursorsPerTab(t *testing.T) {
	ev := &events{}
	r := NewRegistry(ev.emit)
	var ids []string
	var res []*fakeResults
	for range PerOwner + 1 {
		f := &fakeResults{closed: make(chan struct{})}
		res = append(res, f)
		ids = append(ids, r.Start(context.Background(), "tab", func(context.Context) (Results, error) { return f, nil }))
		if _, err := r.Next(context.Background(), ids[len(ids)-1]); err != nil {
			t.Fatal(err)
		}
	}
	<-res[0].closed
	if s := ev.last(ids[0]); s.State != "expired" {
		t.Fatalf("oldest = %+v", s)
	}
	if _, err := r.Next(context.Background(), ids[0]); err == nil {
		t.Fatal("the oldest cursor still answers")
	}
	r.CloseOwner("tab")
	for _, f := range res[1:] {
		<-f.closed
	}
}

// blobResults is one endless result set of a binary column of size-byte
// values.
type blobResults struct{ v []byte }

func (b *blobResults) Columns() []Column          { return []Column{{Name: "b", Binary: true}} }
func (b *blobResults) Next() ([]any, bool, error) { return []any{b.v}, true, nil }
func (b *blobResults) NextResultSet() bool        { return false }
func (b *blobResults) Rerunnable() bool           { return true }
func (b *blobResults) Close() error               { return nil }

// The full values a page keeps for SaveCell count toward its size, so a
// page of large binary cells never holds hundreds of them.
func TestPageBoundsTheFullValuesItHolds(t *testing.T) {
	r := NewRegistry((&events{}).emit)
	const size = 256 << 10
	id := r.Start(context.Background(), "tab", func(context.Context) (Results, error) {
		return &blobResults{v: make([]byte, size)}, nil
	})
	defer r.Close(id)
	p, err := r.Next(context.Background(), id)
	if err != nil || len(p.Rows) == 0 || len(p.Rows)*size > PageHeld+size {
		t.Fatalf("page = %d rows of %d bytes, %v", len(p.Rows), size, err)
	}
}

// A request that runs longer than the idle close is not idle: it is never
// cancelled for it, since user queries have no timeout (KTD23).
func TestRunningQueryOutlivesTheIdleClose(t *testing.T) {
	ev := &events{}
	r := NewRegistry(ev.emit)
	r.idle = 20 * time.Millisecond
	f := &fakeResults{closed: make(chan struct{})}
	id := r.Start(context.Background(), "tab", func(ctx context.Context) (Results, error) {
		select {
		case <-time.After(10 * r.idle):
			return f, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	p, err := r.Next(context.Background(), id)
	if err != nil || len(p.Rows) == 0 {
		t.Fatalf("page = %d rows, %v; state %+v", len(p.Rows), err, ev.last(id))
	}
	r.Close(id)
}

// Cancelling a request that has not started reports "cancelled".
func TestCancelBeforeTheFirstRow(t *testing.T) {
	ev := &events{}
	r := NewRegistry(ev.emit)
	id := r.Start(context.Background(), "tab", func(ctx context.Context) (Results, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	r.Cancel(id)
	if _, err := r.Next(context.Background(), id); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if s := ev.last(id); s.State != "cancelled" {
		t.Fatalf("state = %+v", s)
	}
}
