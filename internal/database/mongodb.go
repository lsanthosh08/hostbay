package database

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MongoConfig is one MongoDB server, from its structured fields only
// (KTD26). Replica-set discovery and mongodb+srv addresses need dials
// beyond the one host, so the connection is direct (KTD24).
type MongoConfig struct {
	Host, User, Password string
	Port                 int
	AuthDB               string // the database the user signs in to, and commands run in by default; "admin" when empty
	TLS                  string // "verify" or "none" (KTD25)
	Dial                 Dial
	// Trust finds the TLS config as tlstrust.Capture does: try connects
	// and signs in; it never runs unverified.
	Trust func(try func(*tls.Config) error) (*tls.Config, error)
}

// mongoDialer hands the driver the dial function, for its monitoring
// connections and every pooled one.
type mongoDialer struct{ dial Dial }

func (d mongoDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.dial(ctx, network, address)
}

// OpenMongo signs in to a MongoDB server over a direct connection to that
// one host, so members a replica set lists are never dialled. The driver
// runs the TLS handshake with Hostbay's config on every connection, so the
// certificate is checked, or the prompt raised, before any sign-in. With
// no user name it signs in to nothing.
func OpenMongo(ctx context.Context, c MongoConfig) (Engine, error) {
	for what, v := range map[string]string{"user name": c.User, "auth database": c.AuthDB} {
		if strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return nil, errors.New("the " + what + " contains a control character")
		}
	}
	if c.Port < 1 || c.Port > 65535 {
		return nil, errors.New("the port must be between 1 and 65535")
	}
	db := c.AuthDB
	if db == "" {
		db = "admin"
	}
	var client *mongo.Client
	try := func(cfg *tls.Config) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		// The driver keeps a failed dial or handshake to itself and retries
		// until it gives up on the server, about 30 seconds later. The
		// first such failure ends the attempt at once and is what it
		// returns, so a jump host, proxy or certificate prompt reaches the
		// page as its own error. Later connections may fail here too, from
		// the driver's goroutines, after this attempt is over.
		var mu sync.Mutex
		var failed error
		fail := func(err error) {
			mu.Lock()
			if failed == nil {
				failed = err
			}
			mu.Unlock()
			cancel()
		}
		dial := withDeadlines(c.Dial)
		opts := options.Client().SetHosts([]string{net.JoinHostPort(c.Host, strconv.Itoa(c.Port))}).SetDirect(true).
			SetDialer(mongoDialer{func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dial(ctx, network, address)
				if err != nil {
					fail(err)
				}
				return conn, err
			}}).SetAppName("Hostbay").SetMaxPoolSize(8).
			// No OCSP responder is asked: that request would leave the chain.
			SetDisableOCSPEndpointCheck(true)
		if cfg != nil {
			cfg = cfg.Clone()
			verify := cfg.VerifyConnection
			cfg.VerifyConnection = func(cs tls.ConnectionState) error {
				err := verify(cs)
				if err != nil {
					fail(err)
				}
				return err
			}
			opts.SetTLSConfig(cfg)
		}
		if c.User != "" {
			opts.SetAuth(options.Credential{Username: c.User, Password: c.Password, PasswordSet: true, AuthSource: db})
		}
		cl, err := mongo.Connect(opts)
		if err != nil {
			return err
		}
		if err := cl.Ping(ctx, nil); err != nil {
			cl.Disconnect(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if failed != nil {
				return failed
			}
			return err
		}
		client = cl
		return nil
	}
	var err error
	if c.TLS == "none" {
		err = try(nil)
	} else {
		_, err = c.Trust(try)
	}
	if err != nil {
		return nil, mongoError(err)
	}
	return &mongoEngine{c: client, db: db}, nil
}

type mongoEngine struct {
	c  *mongo.Client
	db string // the default database
}

func (e *mongoEngine) Close() error { return e.c.Disconnect(context.Background()) }

// mongoError gives a MongoDB error its category.
func mongoError(err error) error {
	if err == nil {
		return nil
	}
	var de *Error
	if errors.As(err, &de) {
		return err
	}
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		switch ce.Code {
		case 18:
			return &Error{Category: "auth", Err: err}
		case 13:
			return &Error{Category: "permission", Err: err}
		case 2, 9, 59, 40324, 40415:
			return &Error{Category: "syntax", Err: err}
		}
	}
	if msg := err.Error(); strings.Contains(msg, "AuthenticationFailed") || strings.Contains(msg, "auth error") {
		return &Error{Category: "auth", Err: err}
	}
	return &Error{Category: Category(err), Err: err}
}

// page is up to TreePage of names from offset, and whether more follow.
func page(names []string, offset int) ([]string, bool) {
	if offset >= len(names) {
		return nil, false
	}
	end := min(offset+TreePage, len(names))
	return names[offset:end], end < len(names)
}

// Children lists databases at the root, then each database's collections.
func (e *mongoEngine) Children(ctx context.Context, path []string, offset int) ([]Node, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, CatalogTimeout)
	defer cancel()
	var names []string
	var err error
	switch len(path) {
	case 0:
		names, err = e.c.ListDatabaseNames(ctx, bson.D{}, options.ListDatabases().SetAuthorizedDatabases(true))
	case 1:
		names, err = e.c.Database(path[0]).ListCollectionNames(ctx, bson.D{})
	default:
		return nil, false, nil
	}
	if err != nil {
		return nil, false, mongoError(err)
	}
	slices.Sort(names)
	names, more := page(names, offset)
	nodes := make([]Node, len(names))
	for i, n := range names {
		if len(path) == 0 {
			nodes[i] = Node{Kind: "database", Name: n, Path: []string{n}, HasChildren: true}
		} else {
			nodes[i] = Node{Kind: "collection", Name: n, Path: []string{path[0], n}}
		}
	}
	return nodes, more, nil
}

// mongoCursors are the commands whose reply is a cursor, paged by getMore.
var mongoCursors = []string{"find", "aggregate", "listCollections", "listIndexes"}

// mongoReadOnly are commands that change nothing.
var mongoReadOnly = []string{"find", "aggregate", "count", "distinct", "listCollections", "listIndexes", "listDatabases",
	"dbStats", "collStats", "ping", "buildInfo", "serverStatus", "hello", "explain"}

// Query runs one database command, written as an Extended JSON document
// and parsed into BSON in Go, in the database its "$db" names, or the
// host's. A command that returns a cursor (find, aggregate) shows a
// document a row and pages through getMore as the results are read.
func (e *mongoEngine) Query(ctx context.Context, text string) (Results, error) {
	var cmd bson.D
	if err := bson.UnmarshalExtJSON([]byte(text), false, &cmd); err != nil {
		return nil, &Error{Category: "syntax", Err: errors.New(`type a database command as an Extended JSON document, such as {"find": "users", "filter": {}}: ` + err.Error())}
	}
	db := e.db
	if i := slices.IndexFunc(cmd, func(el bson.E) bool { return el.Key == "$db" }); i >= 0 {
		name, ok := cmd[i].Value.(string)
		if !ok || name == "" {
			return nil, &Error{Category: "syntax", Err: errors.New(`"$db" names a database: a string`)}
		}
		db, cmd = name, slices.Delete(cmd, i, i+1)
	}
	if len(cmd) == 0 {
		return nil, &Error{Category: "syntax", Err: errors.New("the command is empty")}
	}
	name := cmd[0].Key
	rerun := slices.Contains(mongoReadOnly, name) && !strings.Contains(text, "$out") && !strings.Contains(text, "$merge")
	if slices.Contains(mongoCursors, name) {
		cur, err := e.c.Database(db).RunCommandCursor(ctx, cmd)
		if err != nil {
			return nil, mongoError(err)
		}
		return &memResults{rerun: rerun, close: func() error { return cur.Close(context.Background()) }, sets: []resultSet{{
			cols: []Column{{Name: "_id", Type: "id"}, {Name: "document", Type: "document"}},
			next: func() ([]any, bool, error) {
				if cur.Next(ctx) {
					return []any{mongoID(cur.Current), mongoJSON(cur.Current)}, true, nil
				}
				return nil, false, mongoError(cur.Err())
			},
		}}}, nil
	}
	reply, err := e.c.Database(db).RunCommand(ctx, cmd).Raw()
	if err != nil {
		return nil, mongoError(err)
	}
	return &memResults{rerun: rerun, sets: []resultSet{{cols: []Column{{Name: "reply", Type: "document"}}, next: rowList([][]any{{mongoJSON(reply)}})}}}, nil
}

// mongoJSON is a document as canonical Extended JSON, which keeps every
// type, so an edit sends back an int64 as an int64.
func mongoJSON(doc bson.Raw) string {
	b, err := bson.MarshalExtJSON(doc, true, false)
	if err != nil {
		return doc.String()
	}
	return string(b)
}

// mongoID is a document's _id as canonical Extended JSON, so 1, "1" and
// an ObjectId stay apart; nil when it has none.
func mongoID(doc bson.Raw) any {
	v, err := doc.LookupErr("_id")
	if err != nil {
		return nil
	}
	return idJSON(v)
}

func idJSON(v any) string {
	b, err := bson.MarshalExtJSON(bson.D{{Key: "_id", Value: v}}, true, false)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(b), `{"_id":`), "}")
}

// mongoFilter is the filter matching exactly the _id id names.
func mongoFilter(id string) (bson.D, error) {
	var f bson.D
	if err := bson.UnmarshalExtJSON([]byte(`{"_id":`+id+`}`), false, &f); err != nil || len(f) != 1 || f[0].Key != "_id" {
		return nil, errors.New("that is not a document's _id")
	}
	return f, nil
}

func (e *mongoEngine) EditableTable(ctx context.Context, path []string) (*Table, error) {
	if len(path) != 2 {
		return nil, errors.New("that is not a collection")
	}
	return &Table{Path: path, Key: []string{"_id"}}, nil
}

// SelectTable finds every document of the collection.
func (e *mongoEngine) SelectTable(path []string) string {
	db, _ := json.Marshal(path[0])
	coll, _ := json.Marshal(path[1])
	return `{"$db": ` + string(db) + `, "find": ` + string(coll) + `}`
}

// mongoTarget checks a document's collection path and _id.
func mongoTarget(path []string, id string) (bson.D, error) {
	if len(path) != 2 {
		return nil, errors.New("that is not a collection")
	}
	return mongoFilter(id)
}

func (e *mongoEngine) Document(ctx context.Context, path []string, id string) (string, error) {
	f, err := mongoTarget(path, id)
	if err != nil {
		return "", err
	}
	doc, err := e.c.Database(path[0]).Collection(path[1]).FindOne(ctx, f).Raw()
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", errors.New("this document no longer exists; reload the collection")
	}
	if err != nil {
		return "", mongoError(err)
	}
	return mongoJSON(doc), nil
}

// EditDocument replaces the document by its _id with doc, parsed from
// Extended JSON into typed BSON. The replacement is a $literal value, so a
// field named like an operator ("$set") is stored as a field, never run.
func (e *mongoEngine) EditDocument(ctx context.Context, path []string, id, doc string) error {
	f, err := mongoTarget(path, id)
	if err != nil {
		return err
	}
	var d bson.D
	if err := bson.UnmarshalExtJSON([]byte(doc), false, &d); err != nil {
		return &Error{Category: "syntax", Err: errors.New("the document is not valid Extended JSON: " + err.Error())}
	}
	d, err = withID(d, f[0].Value)
	if err != nil {
		return err
	}
	res, err := e.c.Database(path[0]).Collection(path[1]).UpdateOne(ctx, f, replaceLiteral(d))
	if err != nil {
		return mongoError(err)
	}
	if res.MatchedCount != 1 {
		return errors.New("this document was deleted meanwhile, so nothing was saved; reload the collection")
	}
	return nil
}

// withID is d with its _id, which must be id when d names one.
func withID(d bson.D, id any) (bson.D, error) {
	i := slices.IndexFunc(d, func(el bson.E) bool { return el.Key == "_id" })
	if i < 0 {
		return append(bson.D{{Key: "_id", Value: id}}, d...), nil
	}
	if idJSON(d[i].Value) != idJSON(id) {
		return nil, errors.New("a document's _id cannot be changed")
	}
	return d, nil
}

// replaceLiteral is the update pipeline that replaces a document with
// exactly d.
func replaceLiteral(d bson.D) bson.A {
	return bson.A{bson.D{{Key: "$replaceWith", Value: bson.D{{Key: "$literal", Value: d}}}}}
}
