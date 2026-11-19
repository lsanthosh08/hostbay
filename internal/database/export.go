package database

import (
	"bufio"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Exporter is the optional capability of running a read for an export
// on a connection of its own, never the tab's session: the grid's cursor
// and an open transaction survive it, and it sees committed data only.
type Exporter interface {
	ExportQuery(ctx context.Context, text string) (Results, error)
	ExportTable(ctx context.Context, path []string, b Browse) (Results, error)
}

// exportResults are results on a connection taken for one export.
type exportResults struct {
	*sqlResults
	conn  *sql.Conn
	dirty bool // the connection was set read-only, so it is not reused
}

func (r *exportResults) Close() error {
	err := r.sqlResults.Close()
	if r.dirty {
		_ = r.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	r.conn.Close()
	return err
}

// exportRun runs text with args on a pool connection of its own, held
// read-only on the server too when the tab is (SetReadOnly).
func (e *sqlEngine) exportRun(ctx context.Context, text string, args ...any) (Results, error) {
	e.mu.Lock()
	ro := e.readOnly
	e.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	conn, err := e.db.Conn(ctx)
	if err != nil {
		cancel()
		return nil, e.wrap(err)
	}
	dirty := ro && e.d.readOnly != nil
	if dirty {
		if _, err := conn.ExecContext(ctx, e.d.readOnly(true)); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			conn.Close()
			cancel()
			return nil, e.wrap(err)
		}
	}
	rows, err := conn.QueryContext(ctx, text, args...)
	if err != nil {
		if dirty {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		conn.Close()
		cancel()
		return nil, e.wrap(err)
	}
	r := &sqlResults{e: e, rows: rows, cancel: cancel}
	r.load()
	return &exportResults{sqlResults: r, conn: conn, dirty: dirty}, nil
}

func (e *sqlEngine) ExportQuery(ctx context.Context, text string) (Results, error) {
	return e.exportRun(ctx, text)
}

func (e *sqlEngine) ExportTable(ctx context.Context, path []string, b Browse) (Results, error) {
	q, args, err := e.browse(ctx, path, b, false)
	if err != nil {
		return nil, err
	}
	return e.exportRun(ctx, q, args...)
}

// exportValue is v in full as text, and whether it is NULL: no value is
// cut, and bytes that are not text are hex.
func exportValue(v any, binary bool) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", true
	case []byte:
		if binary || !utf8.Valid(x) {
			return "0x" + hex.EncodeToString(x), false
		}
		return string(x), false
	case string:
		return x, false
	case time.Time:
		return x.Format(time.RFC3339Nano), false
	case bool:
		return strconv.FormatBool(x), false
	case int64:
		return strconv.FormatInt(x, 10), false
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), false
	}
	return fmt.Sprint(v), false
}

// csvSafe keeps a spreadsheet from running a cell as a formula: text that
// starts with = + - @ or a tab or carriage return gets a leading
// apostrophe, unless it is a plain number such as -5.
func csvSafe(s string) string {
	if s == "" || !strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return s
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s
	}
	return "'" + s
}

// firstSet moves r to its first result set that has columns.
func firstSet(r Results) ([]Column, error) {
	for len(r.Columns()) == 0 {
		if !r.NextResultSet() {
			return nil, errors.New("the request returned no rows to export")
		}
	}
	return r.Columns(), nil
}

// Export writes the first result set of r that has columns to w as
// "csv" (a header row, NULL as an empty cell) or "json" (an array of
// objects, keys in column order), every value in full. It returns the
// rows written.
func Export(ctx context.Context, w io.Writer, r Results, format string) (int64, error) {
	cols, err := firstSet(r)
	if err != nil {
		return 0, err
	}
	bw := bufio.NewWriter(w)
	var n int64
	var cw *csv.Writer
	switch format {
	case "csv":
		cw = csv.NewWriter(bw)
		head := make([]string, len(cols))
		for i, c := range cols {
			head[i] = csvSafe(c.Name)
		}
		if err := cw.Write(head); err != nil {
			return 0, err
		}
	case "json":
		bw.WriteString("[")
	default:
		return 0, fmt.Errorf("unknown export format %q", format)
	}
	rec := make([]string, len(cols))
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		vals, ok, err := r.Next()
		if err != nil {
			return n, err
		}
		if !ok {
			break
		}
		if cw != nil {
			for i := range cols {
				s, _ := exportValue(vals[i], cols[i].Binary)
				rec[i] = csvSafe(s)
			}
			if err := cw.Write(rec); err != nil {
				return n, err
			}
		} else {
			if n > 0 {
				bw.WriteString(",")
			}
			bw.WriteString("\n{")
			for i, c := range cols {
				if i > 0 {
					bw.WriteString(",")
				}
				k, _ := json.Marshal(c.Name)
				bw.Write(k)
				bw.WriteString(":")
				bw.Write(jsonValue(vals[i], c.Binary))
			}
			bw.WriteString("}")
		}
		n++
	}
	if cw != nil {
		cw.Flush()
		if err := cw.Error(); err != nil {
			return n, err
		}
	} else {
		bw.WriteString("\n]\n")
	}
	return n, bw.Flush()
}

// jsonValue is v as JSON: numbers and booleans as themselves, NULL as
// null, anything else as a string.
func jsonValue(v any, binary bool) []byte {
	var out any
	switch x := v.(type) {
	case nil:
		return []byte("null")
	case int64, bool:
		out = x
	case float64:
		out = x
	default:
		out, _ = exportValue(v, binary)
	}
	b, err := json.Marshal(out)
	if err != nil { // NaN or an infinity, which JSON has no number for
		b, _ = json.Marshal(fmt.Sprint(v))
	}
	return b
}
