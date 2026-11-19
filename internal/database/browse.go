package database

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Filter is one condition of a table browse: a column, an operator from
// FilterOps, and the value it compares with (unused by the NULL tests).
type Filter struct {
	Column string `json:"column"`
	Op     string `json:"op"`
	Value  string `json:"value"`
}

// FilterOps are the operators a browse takes: a case-insensitive
// substring of the value as text, equality, and the NULL tests.
var FilterOps = []string{"contains", "equals", "null", "notNull"}

// Browse sorts and filters a table opened from the tree. Every filter
// must hold. The zero Browse shows the table as it is. A document
// engine (JSONBrowser) takes Query and Order, a filter and a sort
// written as JSON documents, in place of Filters.
type Browse struct {
	Sort    string   `json:"sort,omitempty"`
	Desc    bool     `json:"desc,omitempty"`
	Filters []Filter `json:"filters,omitempty"`
	Query   string   `json:"query,omitempty"`
	Order   string   `json:"order,omitempty"`
}

// JSONBrowser is a Browser whose filter and sort are JSON documents
// (Browse.Query and Browse.Order), parsed in Go, not column filters.
type JSONBrowser interface {
	Browser
	jsonBrowse()
}

// Browser is the optional capability of browsing a table sorted and
// filtered on the server (KTD23): the page names columns, an operator and
// values, never SQL, and the engine checks each column against the
// table's own catalog before quoting it.
type Browser interface {
	BrowseTable(ctx context.Context, path []string, b Browse) (Results, error)
	CountRows(ctx context.Context, path []string, b Browse) (int64, error)
}

// likeEscape is the escape character of a "contains" filter: one that
// needs no escaping in any dialect's string literal.
const likeEscape = "!"

// columns are path's column names from the catalog, every page of them.
func (e *sqlEngine) columns(ctx context.Context, path []string) ([]string, error) {
	var cols []string
	for off := 0; ; {
		nodes, more, err := e.Children(ctx, path, off)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			if n.Kind == "column" {
				cols = append(cols, n.Name)
			}
		}
		if !more || len(nodes) == 0 {
			return cols, nil
		}
		off += len(nodes)
	}
}

// browseWhere builds b's WHERE clause, its args starting at placeholder
// first, and its ORDER BY, over the table's columns cols.
func (e *sqlEngine) browseWhere(b Browse, cols []string, first int) (where, order string, args []any, err error) {
	known := func(c string) error {
		if !slices.Contains(cols, c) {
			return fmt.Errorf("the table has no column %q", c)
		}
		return nil
	}
	var conds []string
	for _, f := range b.Filters {
		if err := known(f.Column); err != nil {
			return "", "", nil, err
		}
		col := e.d.quote(f.Column)
		ph := func(v any) string {
			args = append(args, v)
			return e.d.placeholder(first + len(args) - 1)
		}
		switch f.Op {
		case "contains":
			if e.d.asText == nil {
				return "", "", nil, errors.New("this database cannot filter by text")
			}
			v := strings.ReplaceAll(f.Value, likeEscape, likeEscape+likeEscape)
			for _, c := range e.d.likeSpecial {
				v = strings.ReplaceAll(v, string(c), likeEscape+string(c))
			}
			conds = append(conds, "LOWER("+e.d.asText(col)+") LIKE LOWER("+ph("%"+v+"%")+") ESCAPE '"+likeEscape+"'")
		case "equals":
			conds = append(conds, col+" = "+ph(f.Value))
		case "null":
			conds = append(conds, col+" IS NULL")
		case "notNull":
			conds = append(conds, col+" IS NOT NULL")
		default:
			return "", "", nil, fmt.Errorf("unknown filter %q", f.Op)
		}
	}
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	if b.Sort != "" {
		if err := known(b.Sort); err != nil {
			return "", "", nil, err
		}
		order = " ORDER BY " + e.d.quote(b.Sort)
		if b.Desc {
			order += " DESC"
		}
	}
	return where, order, args, nil
}

// browse is b's statement over path, after checking path is a table.
func (e *sqlEngine) browse(ctx context.Context, path []string, b Browse, count bool) (string, []any, error) {
	if !e.d.tablePath(path) {
		return "", nil, errors.New("that is not a table")
	}
	if b.Query != "" || b.Order != "" {
		return "", nil, &Error{Category: "syntax", Err: errors.New("filter a table by its columns")}
	}
	var cols []string
	if len(b.Filters) > 0 || b.Sort != "" {
		var err error
		if cols, err = e.columns(ctx, path); err != nil {
			return "", nil, err
		}
	}
	where, order, args, err := e.browseWhere(b, cols, 1)
	if err != nil {
		return "", nil, &Error{Category: "syntax", Err: err}
	}
	if count {
		return "SELECT COUNT(*) FROM " + e.table(path) + where, args, nil
	}
	return e.SelectTable(path) + where + order, args, nil
}

// BrowseTable shows path's rows sorted and filtered by b, on the session,
// paged like any request.
func (e *sqlEngine) BrowseTable(ctx context.Context, path []string, b Browse) (Results, error) {
	q, args, err := e.browse(ctx, path, b, false)
	if err != nil {
		return nil, err
	}
	return e.query(ctx, q, args...)
}

// CountRows counts path's rows that b's filters keep, within the catalog
// timeout, since a count reads the whole table.
func (e *sqlEngine) CountRows(ctx context.Context, path []string, b Browse) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, CatalogTimeout)
	defer cancel()
	q, args, err := e.browse(ctx, path, b, true)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := e.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, e.wrap(err)
	}
	return n, nil
}
