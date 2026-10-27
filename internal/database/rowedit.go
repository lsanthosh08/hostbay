package database

import (
	"context"
	"errors"
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

// EditRow builds the UPDATE in Go from the table's own key, never one the
// page names, with every value a parameter and every identifier quoted,
// and commits only when exactly one row changed (KTD23).
func (e *sqlEngine) EditRow(ctx context.Context, edit RowEdit) error {
	t, err := e.EditableTable(ctx, edit.Path)
	if err != nil {
		return err
	}
	if t == nil {
		return errors.New("this table has no primary or unique key, so it opens read-only")
	}
	if len(edit.Set) == 0 {
		return errors.New("nothing to change")
	}
	if len(edit.Key) != len(t.Key) {
		return errors.New("the row's key does not match the table's key")
	}
	var set, where []string
	var args []any
	arg := func(v *string) string {
		if v == nil {
			args = append(args, nil)
		} else {
			args = append(args, *v)
		}
		return e.d.placeholder(len(args))
	}
	cols := make([]string, 0, len(edit.Set))
	for c := range edit.Set {
		cols = append(cols, c)
	}
	slices.Sort(cols)
	for _, c := range cols {
		set = append(set, e.d.quote(c)+" = "+arg(edit.Set[c]))
	}
	for _, k := range t.Key {
		v, ok := edit.Key[k]
		if !ok || v == nil {
			return errors.New("the row's key does not match the table's key")
		}
		where = append(where, e.d.quote(k)+" = "+arg(v))
	}
	q := "UPDATE " + e.table(t.Path) + " SET " + strings.Join(set, ", ") + " WHERE " + strings.Join(where, " AND ")
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
