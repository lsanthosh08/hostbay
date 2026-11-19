package database

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// table is path's quoted name.
func (e *sqlEngine) table(path []string) string {
	q := make([]string, len(path))
	for i, p := range path {
		q[i] = e.d.quote(p)
	}
	return strings.Join(q, ".")
}

func (e *sqlEngine) SelectTable(path []string) string { return "SELECT * FROM " + e.table(path) }

func (e *sqlEngine) EditableTable(ctx context.Context, path []string) (*Table, error) {
	if !e.d.tablePath(path) {
		return nil, errors.New("that is not a table")
	}
	ctx, cancel := context.WithTimeout(ctx, CatalogTimeout)
	defer cancel()
	key, err := e.d.key(ctx, e.db, path)
	if err != nil || len(key) == 0 {
		return nil, e.wrap(err)
	}
	return &Table{Path: path, Key: key}, nil
}

// RowWriter is the optional capability of inserting and deleting a row
// of a table with a key (KTD23), and of showing the statement any row
// change would run before it runs.
type RowWriter interface {
	InsertRow(ctx context.Context, e RowEdit) error
	DeleteRow(ctx context.Context, e RowEdit) error
	// PreviewRow is the statement op ("update", "insert" or "delete") would
	// run for e, built as it would be, with its values in order.
	PreviewRow(ctx context.Context, op string, e RowEdit) (RowPreview, error)
}

// RowPreview is a row change's statement as Go would run it: placeholders
// in the text, and the values bound to them, in order (nil is NULL).
type RowPreview struct {
	Statement string    `json:"statement"`
	Values    []*string `json:"values"`
}

// rowStatement builds op's statement for edit in Go, from the table's own
// catalog, never from names or SQL the page sends: every identifier is
// checked against the catalog and quoted, every value is a parameter. An
// update and a delete name their row by the table's key.
func (e *sqlEngine) rowStatement(ctx context.Context, op string, edit RowEdit) (string, []any, error) {
	var args []any
	arg := func(v *string) string {
		if v == nil {
			args = append(args, nil)
		} else {
			args = append(args, *v)
		}
		return e.d.placeholder(len(args))
	}
	sorted := func(m map[string]*string) []string {
		cols := make([]string, 0, len(m))
		for c := range m {
			cols = append(cols, c)
		}
		slices.Sort(cols)
		return cols
	}
	if op == "insert" {
		if !e.d.tablePath(edit.Path) {
			return "", nil, errors.New("that is not a table")
		}
		if len(edit.Set) == 0 {
			return "", nil, errors.New("give at least one column a value")
		}
		known, err := e.columns(ctx, edit.Path)
		if err != nil {
			return "", nil, e.wrap(err)
		}
		var names, marks []string
		for _, c := range sorted(edit.Set) {
			if !slices.Contains(known, c) {
				return "", nil, fmt.Errorf("the table has no column %q", c)
			}
			names = append(names, e.d.quote(c))
			marks = append(marks, arg(edit.Set[c]))
		}
		return "INSERT INTO " + e.table(edit.Path) + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")", args, nil
	}
	t, err := e.EditableTable(ctx, edit.Path)
	if err != nil {
		return "", nil, err
	}
	if t == nil {
		return "", nil, errors.New("this table has no primary or unique key, so its rows cannot be changed here")
	}
	if len(edit.Key) != len(t.Key) {
		return "", nil, errors.New("the row's key does not match the table's key")
	}
	var set, where []string
	if op == "update" {
		if len(edit.Set) == 0 {
			return "", nil, errors.New("nothing to change")
		}
		for _, c := range sorted(edit.Set) {
			set = append(set, e.d.quote(c)+" = "+arg(edit.Set[c]))
		}
	}
	for _, k := range t.Key {
		v, ok := edit.Key[k]
		if !ok || v == nil {
			return "", nil, errors.New("the row's key does not match the table's key")
		}
		where = append(where, e.d.quote(k)+" = "+arg(v))
	}
	switch op {
	case "update":
		return "UPDATE " + e.table(t.Path) + " SET " + strings.Join(set, ", ") + " WHERE " + strings.Join(where, " AND "), args, nil
	case "delete":
		return "DELETE FROM " + e.table(t.Path) + " WHERE " + strings.Join(where, " AND "), args, nil
	}
	return "", nil, fmt.Errorf("unknown row change %q", op)
}

func (e *sqlEngine) PreviewRow(ctx context.Context, op string, edit RowEdit) (RowPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, CatalogTimeout)
	defer cancel()
	q, args, err := e.rowStatement(ctx, op, edit)
	if err != nil {
		return RowPreview{}, err
	}
	p := RowPreview{Statement: q, Values: make([]*string, len(args))}
	for i, a := range args {
		if s, ok := a.(string); ok {
			p.Values[i] = &s
		}
	}
	return p, nil
}

// EditRow builds the UPDATE in Go from the table's own key, never one the
// page names, with every value a parameter and every identifier quoted,
// and commits only when exactly one row changed (KTD23).
func (e *sqlEngine) EditRow(ctx context.Context, edit RowEdit) error {
	return e.writeRow(ctx, "update", edit)
}

// DeleteRow deletes the one row the table's key names, and commits only
// when exactly one row went.
func (e *sqlEngine) DeleteRow(ctx context.Context, edit RowEdit) error {
	return e.writeRow(ctx, "delete", edit)
}

// InsertRow inserts one row: the columns edit.Set names, each checked
// against the catalog; the others take their defaults.
func (e *sqlEngine) InsertRow(ctx context.Context, edit RowEdit) error {
	return e.writeRow(ctx, "insert", edit)
}

// writeRow runs op's statement in a transaction and commits only when
// exactly one row was affected.
func (e *sqlEngine) writeRow(ctx context.Context, op string, edit RowEdit) error {
	q, args, err := e.rowStatement(ctx, op, edit)
	if err != nil {
		return err
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return e.wrap(err)
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		tx.Rollback()
		return e.wrap(err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		tx.Rollback()
		if err != nil {
			return e.wrap(err)
		}
		if n == 0 {
			return errors.New("the row was changed or deleted meanwhile, so nothing was saved; reload the table")
		}
		return errors.New(strconv.FormatInt(n, 10) + " rows matched the key, so nothing was saved")
	}
	return e.wrap(tx.Commit())
}
