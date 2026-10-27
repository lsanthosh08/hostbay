// Package database is Hostbay's database client (R56, KTD23). Every engine
// implements one narrow interface: list a node's children, run a request
// that returns results, and close. Row editing is an optional capability
// found by type assertion. Results reach the page as pages pulled by a
// backend-minted query ID, never as a stream of events.
package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// Limits from KTD23.
const (
	PageRows       = 500              // rows in one page at most
	PageBytes      = 1 << 20          // encoded cell bytes in one page at most
	PageHeld       = 32 << 20         // full values of cut and binary cells a page keeps
	CellCap        = 64 << 10         // a longer cell is cut, with a marker
	TreePage       = 200              // children per tree page, then "load more"
	CatalogTimeout = 10 * time.Second // the tree's own catalog queries
	IdleClose      = 60 * time.Second // an untouched cursor closes
	PerOwner       = 4                // open cursors per database tab
	binaryPrefix   = 32               // bytes of a binary value shown as hex
)

// Node is one entry of the lazy object tree: schema, table, view or column
// for SQL. Path names it from the root and is what Children takes.
type Node struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Path        []string `json:"path"`
	HasChildren bool     `json:"hasChildren"`
	Detail      string   `json:"detail,omitempty"` // a column's type
}

// Column is one column of a result set. Binary columns show as size plus
// a hex prefix.
type Column struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Binary bool   `json:"binary,omitempty"`
}

// Cell is one value as text. A cut or binary cell carries the full value's
// size; "load full value" writes it to a file (SaveCell).
type Cell struct {
	Text   string `json:"t"`
	Null   bool   `json:"n,omitempty"`
	Cut    bool   `json:"c,omitempty"`
	Binary bool   `json:"b,omitempty"`
	Size   int    `json:"s,omitempty"`
}

// Page is the next part of a request's results. Columns come with the
// first page of each result set; Row is the index of the page's first row
// in its result set. Done means nothing follows.
type Page struct {
	Result  int      `json:"result"`
	Columns []Column `json:"columns,omitempty"`
	Row     int      `json:"row"`
	Rows    [][]Cell `json:"rows"`
	Done    bool     `json:"done"`
}

// Engine is one open database. Children's catalog queries are bounded by
// CatalogTimeout by the caller; Query has no timeout.
type Engine interface {
	// Children lists up to TreePage children of path (nil: the root) from
	// offset, and whether more follow.
	Children(ctx context.Context, path []string, offset int) (nodes []Node, more bool, err error)
	// Query runs a request: the selection, or the whole editor, as one.
	// Cancelling ctx cancels it through the engine's own cancel.
	Query(ctx context.Context, text string) (Results, error)
	Close() error
}

// Results are a request's result sets, read a row at a time.
type Results interface {
	// Columns of the current result set; none for a statement without rows.
	Columns() []Column
	// Next returns the current result set's next row, or false at its end.
	Next() ([]any, bool, error)
	// NextResultSet moves to the next result set, or reports there is none.
	NextResultSet() bool
	// Rerunnable reports a request that, judged by its text, changes
	// nothing, so running it again after its results expired is harmless.
	Rerunnable() bool
	Close() error
}

// Table names a table that can be edited by its key.
type Table struct {
	Path []string `json:"path"` // schema and table, as the tree names them
	Key  []string `json:"key"`  // the primary or unique key's columns
	// Edit lists the only columns EditRow changes; empty means every one.
	Edit []string `json:"edit,omitempty"`
	// NoNull is set when EditRow refuses NULL in every column.
	NoNull bool `json:"noNull,omitempty"`
}

// RowEdit changes one row: Key holds the key columns' current values, Set
// the new values (nil is NULL).
type RowEdit struct {
	Path []string           `json:"path"`
	Key  map[string]*string `json:"key"`
	Set  map[string]*string `json:"set"`
}

// RowEditor is the optional row-editing capability (KTD23).
type RowEditor interface {
	// EditableTable returns path's key, or nil when it has none and opens
	// read-only.
	EditableTable(ctx context.Context, path []string) (*Table, error)
	// SelectTable is the request that shows path's rows.
	SelectTable(path []string) string
	// EditRow changes exactly one row in a transaction, or rolls back.
	EditRow(ctx context.Context, e RowEdit) error
}

// DocumentEditor is the optional document-editing capability of MongoDB
// and Elasticsearch (KTD23): a collection or index opens like a table
// whose rows are documents, each named by its "_id" cell, and a document
// is changed whole, by that _id, as JSON.
type DocumentEditor interface {
	// EditableTable returns path's key, ["_id"].
	EditableTable(ctx context.Context, path []string) (*Table, error)
	// SelectTable is the request that shows path's documents.
	SelectTable(path []string) string
	// Document returns the document id names in full, as JSON.
	Document(ctx context.Context, path []string, id string) (string, error)
	// EditDocument replaces the document id names with doc, or fails when
	// it no longer exists or doc names another _id.
	EditDocument(ctx context.Context, path []string, id, doc string) error
}

// Capabilities lists what e offers the page beyond browsing and queries;
// the page reads these and never branches on the engine name.
func Capabilities(e Engine) []string {
	caps := []string{}
	if _, ok := e.(RowEditor); ok {
		caps = append(caps, "rowEdit")
	}
	if _, ok := e.(DocumentEditor); ok {
		caps = append(caps, "documentEdit")
	}
	return caps
}

// resultSet is one result set of memResults: its columns and its rows,
// read one at a time, so a large value can be fetched as it is paged.
type resultSet struct {
	cols []Column
	next func() ([]any, bool, error)
}

// rowList serves rows already in memory.
func rowList(rows [][]any) func() ([]any, bool, error) {
	return func() ([]any, bool, error) {
		if len(rows) == 0 {
			return nil, false, nil
		}
		r := rows[0]
		rows = rows[1:]
		return r, true, nil
	}
}

// memResults are the results of an engine without database/sql rows.
type memResults struct {
	sets  []resultSet
	rerun bool
	close func() error // a server-side cursor's, when one is open
}

func (r *memResults) Columns() []Column {
	if len(r.sets) == 0 {
		return nil
	}
	return r.sets[0].cols
}

func (r *memResults) Next() ([]any, bool, error) {
	if len(r.sets) == 0 {
		return nil, false, nil
	}
	return r.sets[0].next()
}

func (r *memResults) NextResultSet() bool {
	if len(r.sets) > 0 {
		r.sets = r.sets[1:]
	}
	return len(r.sets) > 0
}

func (r *memResults) Rerunnable() bool { return r.rerun }

func (r *memResults) Close() error {
	if r.close == nil {
		return nil
	}
	return r.close()
}

// State is a query-state event: running, done, error, cancelled or
// expired. Rerun is set on an expired request it is safe to run again.
type State struct {
	QueryID string `json:"queryId"`
	State   string `json:"state"`
	Rows    int    `json:"rows"`
	Message string `json:"message,omitempty"`
	Rerun   bool   `json:"rerun,omitempty"`
}

// stateEvery is how often a running query reports at most.
const stateEvery = 100 * time.Millisecond

// ErrExpired is a cursor closed for idleness or by a newer request.
var ErrExpired = errors.New("these results expired; run the query again")

// Registry holds every open cursor by query ID, at most PerOwner per tab,
// closing the oldest beyond that and any left idle for IdleClose.
type Registry struct {
	emit func(State)
	idle time.Duration

	mu      sync.Mutex
	cursors map[string]*cursor
}

func NewRegistry(emit func(State)) *Registry {
	return &Registry{emit: emit, idle: IdleClose, cursors: map[string]*cursor{}}
}

type cursor struct {
	id, owner string
	opened    time.Time
	cancel    context.CancelFunc
	ready     chan struct{} // closed once the request has started or failed

	mu       sync.Mutex
	res      Results
	err      error
	result   int  // the current result set
	row      int  // rows already read from it
	sentCols bool // the current set's columns went out
	rows     int  // rows seen in all
	done     bool
	expired  bool
	noRows   bool // a result set returned no rows
	full     map[[3]int][]byte
	idle     *time.Timer
	lastEmit time.Time
}

// Start runs a request for owner and returns its query ID at once; Next
// waits for it to start. ctx bounds the cursor's whole life.
func (r *Registry) Start(ctx context.Context, owner string, run func(context.Context) (Results, error)) string {
	ctx, cancel := context.WithCancel(ctx)
	c := &cursor{id: newID(), owner: owner, opened: time.Now(), cancel: cancel, ready: make(chan struct{}), full: map[[3]int][]byte{}}
	r.mu.Lock()
	var oldest *cursor
	n := 0
	for _, o := range r.cursors {
		if o.owner == owner {
			n++
			if oldest == nil || o.opened.Before(oldest.opened) {
				oldest = o
			}
		}
	}
	if n >= PerOwner {
		delete(r.cursors, oldest.id)
	}
	r.cursors[c.id] = c
	r.mu.Unlock()
	if n >= PerOwner {
		r.expire(oldest)
	}
	r.state(c, "running", "", true)
	// The idle clock starts once the request has started: a running query
	// is not idle, and user queries have no timeout (KTD23).
	c.idle = time.AfterFunc(r.idle, func() { r.expire(c) })
	c.idle.Stop()
	go func() {
		res, err := run(ctx)
		c.mu.Lock()
		c.res, c.err = res, err
		if err == nil && ctx.Err() == nil { // not closed meanwhile
			c.idle.Reset(r.idle)
		}
		c.mu.Unlock()
		close(c.ready)
		if err != nil {
			r.finish(c, err)
		}
	}()
	return c.id
}

func (r *Registry) get(id string) (*cursor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.cursors[id]; c != nil {
		return c, nil
	}
	return nil, errors.New("no query with that ID")
}

// Next returns the next page: up to PageRows rows, PageBytes of cells and
// PageHeld of full values from one result set.
func (r *Registry) Next(ctx context.Context, id string) (Page, error) {
	c, err := r.get(id)
	if err != nil {
		return Page{}, err
	}
	select {
	case <-c.ready:
	case <-ctx.Done():
		return Page{}, ctx.Err()
	}
	p, done, fresh, err := c.page(r.idle)
	switch {
	case fresh:
		r.finish(c, err)
	case done:
		r.state(c, "done", "", true)
	case err == nil:
		r.state(c, "running", "", false)
	}
	return p, err
}

// page reads c's next page. done reports the end of the last result set;
// fresh an error this read hit, as opposed to one already reported.
func (c *cursor) page(idle time.Duration) (p Page, done, fresh bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.err != nil:
		return Page{}, false, false, c.err
	case c.expired:
		return Page{}, false, false, ErrExpired
	case c.done:
		return Page{Result: c.result, Done: true, Rows: [][]Cell{}}, false, false, nil
	}
	c.idle.Reset(idle)
	p = Page{Result: c.result, Row: c.row, Rows: [][]Cell{}}
	cols := c.res.Columns()
	if !c.sentCols {
		p.Columns, c.sentCols = cols, true
		c.noRows = c.noRows || len(cols) == 0
	}
	clear(c.full)
	size, held := 0, 0
	for len(p.Rows) < PageRows && size < PageBytes && held < PageHeld {
		vals, ok, err := c.res.Next()
		if err != nil {
			c.err = err
			return Page{}, false, true, err
		}
		if !ok {
			if c.res.NextResultSet() {
				c.result, c.row, c.sentCols = c.result+1, 0, false
				return p, false, false, nil
			}
			c.done, p.Done = true, true
			_ = c.res.Close()
			return p, true, false, nil
		}
		row := make([]Cell, len(vals))
		for i, v := range vals {
			cell, full := encode(v, i < len(cols) && cols[i].Binary)
			if full != nil {
				c.full[[3]int{c.result, c.row, i}] = full
				held += len(full)
			}
			size += len(cell.Text)
			row[i] = cell
		}
		p.Rows = append(p.Rows, row)
		c.row++
		c.rows++
	}
	return p, false, false, nil
}

// SaveCell writes the full value of a cut or binary cell of the page last
// read to w.
func (r *Registry) SaveCell(id string, result, row, col int, w io.Writer) error {
	c, err := r.get(id)
	if err != nil {
		return err
	}
	c.mu.Lock()
	v, ok := c.full[[3]int{result, row, col}]
	c.mu.Unlock()
	if !ok {
		return errors.New("that value is no longer held; load the page again")
	}
	_, err = w.Write(v)
	return err
}

// Cancel cancels a running request through the engine's own cancel.
func (r *Registry) Cancel(id string) {
	if c, err := r.get(id); err == nil {
		c.cancel()
	}
}

// Close forgets a cursor, cancelling it if it still runs. Unknown IDs are
// already closed.
func (r *Registry) Close(id string) {
	r.mu.Lock()
	c := r.cursors[id]
	delete(r.cursors, id)
	r.mu.Unlock()
	if c != nil {
		r.release(c)
	}
}

// CloseOwner closes every cursor of a closing tab.
func (r *Registry) CloseOwner(owner string) {
	r.mu.Lock()
	var mine []*cursor
	for id, c := range r.cursors {
		if c.owner == owner {
			mine = append(mine, c)
			delete(r.cursors, id)
		}
	}
	r.mu.Unlock()
	for _, c := range mine {
		r.release(c)
	}
}

// release cancels c and closes its results; a cursor that is not done
// cancels first, so closing never waits for the rest of a long result.
func (r *Registry) release(c *cursor) {
	c.cancel()
	c.idle.Stop()
	go func() {
		<-c.ready
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.res != nil && !c.done {
			_ = c.res.Close()
		}
		c.done = true
	}()
}

// expire closes c for idleness or to make room, keeping its ID so the tab
// learns its results expired, and whether a re-run is harmless.
func (r *Registry) expire(c *cursor) {
	c.mu.Lock()
	if c.done || c.expired || c.err != nil {
		c.mu.Unlock()
		return
	}
	c.expired = true
	rerun := false
	select {
	case <-c.ready:
		rerun = c.res != nil && c.res.Rerunnable() && !c.noRows
	default:
	}
	c.mu.Unlock()
	r.release(c)
	r.emit(State{QueryID: c.id, State: "expired", Rows: c.rows, Message: ErrExpired.Error(), Rerun: rerun})
}

// finish ends c on err: "cancelled" when its context was cancelled.
func (r *Registry) finish(c *cursor, err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
	if errors.Is(err, context.Canceled) || Category(err) == "cancelled" {
		r.state(c, "cancelled", "", true)
		return
	}
	r.state(c, "error", err.Error(), true)
}

// state emits a query-state event; running ones at most every stateEvery.
func (r *Registry) state(c *cursor, state, msg string, force bool) {
	c.mu.Lock()
	now := time.Now()
	if !force && now.Sub(c.lastEmit) < stateEvery {
		c.mu.Unlock()
		return
	}
	c.lastEmit = now
	s := State{QueryID: c.id, State: state, Rows: c.rows, Message: msg}
	c.mu.Unlock()
	r.emit(s)
}

// encode renders v as text. A binary value shows its size and a hex
// prefix, and a text over CellCap is cut; either returns the full value
// for SaveCell.
func encode(v any, binary bool) (Cell, []byte) {
	var s string
	switch x := v.(type) {
	case nil:
		return Cell{Null: true}, nil
	case []byte:
		if binary || !utf8.Valid(x) {
			n := min(len(x), binaryPrefix)
			full := append([]byte(nil), x...)
			return Cell{Text: hex.EncodeToString(x[:n]), Binary: true, Size: len(x)}, full
		}
		s = string(x)
	case string:
		s = x
	case time.Time:
		s = x.Format(time.RFC3339Nano)
	case bool:
		s = strconv.FormatBool(x)
	case int64:
		s = strconv.FormatInt(x, 10)
	case float64:
		s = strconv.FormatFloat(x, 'g', -1, 64)
	default:
		s = fmt.Sprint(x)
	}
	if len(s) <= CellCap {
		return Cell{Text: s}, nil
	}
	cut := s[:CellCap]
	for !utf8.ValidString(cut) { // never split a character
		cut = cut[:len(cut)-1]
	}
	return Cell{Text: cut, Cut: true, Size: len(s)}, []byte(s)
}

// newID returns an opaque random ID.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return hex.EncodeToString(b[:])
}
