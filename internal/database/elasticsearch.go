package database

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"hostbay/internal/transfer"
)

// ESConfig is an Elasticsearch node, by its URL (KTD24). Nodes are never
// sniffed: every request goes to this URL, and a redirect elsewhere fails.
type ESConfig struct {
	URL, User, Password string
	Dial                Dial
	// Trust finds the TLS config of an https:// URL as tlstrust.Capture
	// does; it never runs unverified.
	Trust func(try func(*tls.Config) error) (*tls.Config, error)
}

// esMaxBody is the most of a response Hostbay reads; the rest is cut.
const esMaxBody = 1 << 20

// esMethods are the methods the editor sends.
var esMethods = []string{"GET", "POST", "PUT", "DELETE", "HEAD"}

// OpenElasticsearch signs in to an Elasticsearch node over
// transfer.HTTPClient: through the dial function only, with no environment
// proxy, redirects only to the same origin, and for https:// the
// certificate checked, or the prompt raised, before the password is sent.
func OpenElasticsearch(ctx context.Context, c ESConfig) (Engine, error) {
	u, err := url.Parse(c.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("enter the node's URL, such as https://search.example:9200")
	}
	e := &esEngine{base: u, user: c.User, password: c.Password}
	try := func(cfg *tls.Config) error {
		// shortcut: HTTPClient waits at most a minute for response headers,
		// so a request slower than that fails; give database hosts a client
		// without that limit when users run long requests.
		e.client = transfer.HTTPClient(cfg, u, transfer.DialFunc(c.Dial))
		_, _, _, err := e.call(ctx, "GET", nil, "", nil)
		return err
	}
	if u.Scheme == "http" {
		err = try(nil)
	} else {
		_, err = c.Trust(try)
	}
	if err != nil {
		e.closeIdle()
		return nil, esError(err)
	}
	return e, nil
}

type esEngine struct {
	base           *url.URL
	user, password string
	client         *http.Client
}

func (e *esEngine) closeIdle() {
	if e.client != nil {
		e.client.CloseIdleConnections()
	}
}

func (e *esEngine) Close() error { e.closeIdle(); return nil }

func esError(err error) error { return categorized(err, Category) }

// esPath splits a typed path into its segments, decoded. A "." or ".."
// segment, written plainly or percent-escaped, is refused, so a request
// never leaves the path it names.
func esPath(raw string) ([]string, error) {
	var segs []string
	for _, s := range strings.Split(raw, "/") {
		if s == "" {
			continue
		}
		d, err := url.PathUnescape(s)
		if err != nil {
			return nil, fmt.Errorf("the path segment %q is not valid", s)
		}
		if d == "." || d == ".." {
			return nil, errors.New(`a path with a ".." or "." segment is refused`)
		}
		segs = append(segs, d)
	}
	return segs, nil
}

// call sends one request to the path segs names under the URL, each
// segment escaped whole, so a "/" inside an index name or _id stays in its
// segment. It returns the status and at most esMaxBody of the body, and
// whether more was cut. An error status is an error.
func (e *esEngine) call(ctx context.Context, method string, segs []string, query string, body []byte) (int, []byte, bool, error) {
	u := *e.base
	esc := make([]string, len(segs))
	for i, s := range segs {
		esc[i] = url.PathEscape(s)
	}
	basePath, baseRaw := strings.TrimSuffix(u.Path, "/"), strings.TrimSuffix(u.EscapedPath(), "/")
	u.Path, u.RawPath = basePath+"/"+strings.Join(segs, "/"), baseRaw+"/"+strings.Join(esc, "/")
	q, err := url.ParseQuery(query)
	if err != nil {
		return 0, nil, false, &Error{Category: "syntax", Err: errors.New("the query string is not valid")}
	}
	u.RawQuery = q.Encode()
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return 0, nil, false, err
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.user != "" {
		req.SetBasicAuth(e.user, e.password)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, nil, false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, esMaxBody+1))
	if err != nil {
		return 0, nil, false, err
	}
	cut := len(data) > esMaxBody
	if cut {
		data = data[:esMaxBody]
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, data, cut, esStatus(resp.StatusCode, data)
	}
	return resp.StatusCode, data, cut, nil
}

// statusError is an error status with Elasticsearch's own error type.
type statusError struct {
	code      int
	typ, text string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("Elasticsearch answered %d %s: %s", e.code, http.StatusText(e.code), e.text)
}

// esStatus is an error status as an error with its category, carrying
// Elasticsearch's own type and reason when the body has them.
func esStatus(code int, body []byte) error {
	var r struct {
		Error struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	se := &statusError{code: code, text: strings.TrimSpace(string(body))}
	if json.Unmarshal(body, &r) == nil && r.Error.Type != "" {
		se.typ, se.text = r.Error.Type, r.Error.Type+": "+r.Error.Reason
	}
	if len(se.text) > 2000 {
		se.text = se.text[:2000] + "…"
	}
	cat := map[int]string{400: "syntax", 401: "auth", 403: "permission", 408: "timeout", 504: "timeout"}[code]
	return &Error{Category: cat, Err: se}
}

// isStatus reports an error status code, other than one of Elasticsearch's
// error types except.
func isStatus(err error, code int, except string) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == code && se.typ != except
}

// Children pages the indices, from _cat/indices, at the root.
func (e *esEngine) Children(ctx context.Context, path []string, offset int) ([]Node, bool, error) {
	if len(path) > 0 {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, CatalogTimeout)
	defer cancel()
	_, data, cut, err := e.call(ctx, "GET", []string{"_cat", "indices"}, "format=json&h=index,docs.count&s=index", nil)
	if err == nil && cut {
		err = errors.New("the index list is larger than 1 MiB")
	}
	if err != nil {
		return nil, false, esError(err)
	}
	var list []struct {
		Index string `json:"index"`
		Docs  string `json:"docs.count"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, false, fmt.Errorf("the index list did not decode: %w", err)
	}
	from := min(offset, len(list))
	end := min(from+TreePage, len(list))
	nodes := make([]Node, 0, end-from)
	for _, ix := range list[from:end] {
		nodes = append(nodes, Node{Kind: "index", Name: ix.Index, Path: []string{ix.Index}, Detail: ix.Docs})
	}
	return nodes, end < len(list), nil
}

// Query sends one request: a first line of a method and a path, such as
// "GET /logs/_search?q=error", then an optional JSON body. A search's hits
// show a document a row; any other response shows as its status and body,
// cut at 1 MiB.
func (e *esEngine) Query(ctx context.Context, text string) (Results, error) {
	first, body, _ := strings.Cut(strings.TrimSpace(text), "\n")
	f := strings.Fields(first)
	if len(f) != 2 || !slices.Contains(esMethods, strings.ToUpper(f[0])) {
		return nil, &Error{Category: "syntax", Err: errors.New("start with a method (GET, POST, PUT, DELETE or HEAD) and a path, such as GET /_cluster/health")}
	}
	method := strings.ToUpper(f[0])
	rawPath, query, _ := strings.Cut(f[1], "?")
	segs, err := esPath(rawPath)
	if err != nil {
		return nil, &Error{Category: "syntax", Err: err}
	}
	var data []byte
	if b := strings.TrimSpace(body); b != "" {
		data = []byte(b)
	}
	status, resp, cut, err := e.call(ctx, method, segs, query, data)
	if err != nil {
		return nil, esError(err)
	}
	last := ""
	if len(segs) > 0 {
		last = segs[len(segs)-1]
	}
	rerun := method == "GET" || method == "HEAD" || (method == "POST" && (strings.HasPrefix(last, "_search") || last == "_count"))
	if hits, ok := esHits(resp, cut); ok {
		return &memResults{rerun: rerun, sets: []resultSet{hits}}, nil
	}
	text = string(resp)
	if cut {
		text += "\n… (the response was cut at 1 MiB)"
	}
	return &memResults{rerun: rerun, sets: []resultSet{{
		cols: []Column{{Name: "status", Type: "integer"}, {Name: "response", Type: "json"}},
		next: rowList([][]any{{int64(status), text}}),
	}}}, nil
}

// esHits is a search response's hits as rows, a document's _source to a
// row; ok is false for any other response. Of a response cut at
// esMaxBody, the hits that arrived whole are kept.
func esHits(resp []byte, cut bool) (resultSet, bool) {
	if !cut && !json.Valid(resp) {
		return resultSet{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(resp))
	if !esEnter(dec, "hits") || !esEnter(dec, "hits") {
		return resultSet{}, false
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return resultSet{}, false
	}
	rows := [][]any{}
	for dec.More() {
		var h struct {
			Index  string          `json:"_index"`
			ID     string          `json:"_id"`
			Source json.RawMessage `json:"_source"`
		}
		if dec.Decode(&h) != nil {
			break // the cut
		}
		var src any
		if len(h.Source) > 0 {
			src = string(h.Source)
		}
		rows = append(rows, []any{h.Index, h.ID, src})
	}
	if cut && len(rows) == 0 {
		return resultSet{}, false
	}
	return resultSet{cols: []Column{{Name: "_index", Type: "index"}, {Name: "_id", Type: "id"}, {Name: "document", Type: "document"}}, next: rowList(rows)}, true
}

// esEnter reads an object up to the value of its key, skipping the others.
func esEnter(dec *json.Decoder, key string) bool {
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return false
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return false
		}
		if k == key {
			return true
		}
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return false
		}
	}
	return false
}

func (e *esEngine) EditableTable(ctx context.Context, path []string) (*Table, error) {
	if len(path) != 1 {
		return nil, errors.New("that is not an index")
	}
	return &Table{Path: path, Key: []string{"_id"}}, nil
}

// SelectTable searches the index for its first 500 documents.
// shortcut: from and size reach only the first page; page further with
// search_after when an index view needs more than 500 documents.
func (e *esEngine) SelectTable(path []string) string {
	return "GET /" + url.PathEscape(path[0]) + "/_search?size=" + strconv.Itoa(PageRows)
}

// esDoc is a document as GET _doc returns it.
type esDoc struct {
	Found       bool            `json:"found"`
	SeqNo       int64           `json:"_seq_no"`
	PrimaryTerm int64           `json:"_primary_term"`
	Source      json.RawMessage `json:"_source"`
}

func (e *esEngine) get(ctx context.Context, path []string, id string) (esDoc, error) {
	if len(path) != 1 {
		return esDoc{}, errors.New("that is not an index")
	}
	var d esDoc
	_, data, cut, err := e.call(ctx, "GET", []string{path[0], "_doc", id}, "", nil)
	if isStatus(err, 404, "index_not_found_exception") {
		return esDoc{}, errors.New("this document no longer exists; reload the index")
	}
	if err == nil && cut {
		err = errors.New("the document is larger than 1 MiB")
	}
	if err != nil {
		return esDoc{}, esError(err)
	}
	if err := json.Unmarshal(data, &d); err != nil || !d.Found {
		return esDoc{}, errors.New("this document no longer exists; reload the index")
	}
	return d, nil
}

func (e *esEngine) Document(ctx context.Context, path []string, id string) (string, error) {
	d, err := e.get(ctx, path, id)
	return string(d.Source), err
}

// EditDocument replaces the document by its _id, on condition that it is
// the version just read, so one deleted or changed meanwhile is neither
// recreated nor overwritten.
func (e *esEngine) EditDocument(ctx context.Context, path []string, id, doc string) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &obj); err != nil {
		return &Error{Category: "syntax", Err: errors.New("the document must be a JSON object: " + err.Error())}
	}
	d, err := e.get(ctx, path, id)
	if err != nil {
		return err
	}
	q := url.Values{"if_seq_no": {strconv.FormatInt(d.SeqNo, 10)}, "if_primary_term": {strconv.FormatInt(d.PrimaryTerm, 10)}, "refresh": {"wait_for"}}
	_, _, _, err = e.call(ctx, "PUT", []string{path[0], "_doc", id}, q.Encode(), []byte(doc))
	if isStatus(err, 409, "") {
		return errors.New("this document was changed meanwhile, so nothing was saved; reload the index")
	}
	return esError(err)
}
