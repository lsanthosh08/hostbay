package database

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// RedisConfig is a standalone Redis server, from its structured fields only
// (KTD26). Redis Cluster and Sentinel need dials beyond the one host, so
// they are not supported (KTD24).
type RedisConfig struct {
	Host, User, Password string
	Port                 int
	DB                   int    // the database number
	TLS                  string // "verify" (rediss) or "none" (KTD25)
	Dial                 Dial
	// Trust finds the TLS config as tlstrust.Capture does: try connects
	// and signs in; it never runs unverified.
	Trust func(try func(*tls.Config) error) (*tls.Config, error)
}

// OpenRedis signs in to a Redis server. Every connection goes through
// c.Dial, and for "verify" the dial itself runs the TLS handshake, because
// go-redis ignores its TLSConfig once a Dialer is set; so the server's
// certificate is checked, or the prompt raised, before AUTH is sent. A
// password without a user name signs in as AUTH <password>.
func OpenRedis(ctx context.Context, c RedisConfig) (Engine, error) {
	if strings.IndexFunc(c.User, unicode.IsControl) >= 0 {
		return nil, errors.New("the user name contains a control character")
	}
	if c.Port < 1 || c.Port > 65535 {
		return nil, errors.New("the port must be between 1 and 65535")
	}
	if c.DB < 0 {
		return nil, errors.New("the Redis database number cannot be negative")
	}
	var client *redis.Client
	try := func(cfg *tls.Config) error {
		cl := redis.NewClient(&redis.Options{
			Addr: net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), Username: c.User, Password: c.Password, DB: c.DB,
			// RESP2 has no push messages, so the server cannot send the
			// client elsewhere; maintenance notifications would.
			Protocol: 2, MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
			DisableIdentity: true, PoolSize: 4, DialerRetries: 1,
			// A failed command is never sent again on its own.
			MaxRetries: -1,
			// Commands the user runs have no timeout; the tree's carry
			// theirs in their context.
			ReadTimeout: -1, WriteTimeout: -1, ContextTimeoutEnabled: true,
			Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := withDeadlines(c.Dial)(ctx, network, addr)
				if err != nil || cfg == nil {
					return conn, err
				}
				tc := tls.Client(conn, cfg)
				if err := tc.HandshakeContext(ctx); err != nil {
					conn.Close()
					return nil, err
				}
				return tc, nil
			},
		})
		if err := cl.Ping(ctx).Err(); err != nil {
			cl.Close()
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
		return nil, redisError(err)
	}
	return &redisEngine{c: client, types: map[string]string{}}, nil
}

// redisEngine browses one database's keys and runs commands. Each command
// runs on any pooled connection, so commands that change a connection's
// own state are refused (redisRefused).
type redisEngine struct {
	c *redis.Client

	mu    sync.Mutex
	scan  redisScan
	types map[string]string // key types the tree or a table open saw
}

// redisScan is where the tree's SCAN stands: the cursor, the keys it
// returned beyond the last page, and the offset the next page starts at.
type redisScan struct {
	offset int
	cursor uint64
	keys   []string
	done   bool
}

// redisError gives a Redis error its category. A Cluster redirect is not
// followed (the client is standalone); it fails here, naming where the
// server pointed (KTD24).
func redisError(err error) error {
	if err == nil || errors.Is(err, redis.Nil) {
		return err
	}
	var de *Error
	if errors.As(err, &de) {
		return err
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "MOVED ") || strings.HasPrefix(msg, "ASK "):
		f := strings.Fields(msg)
		return &Error{Category: "network", Err: fmt.Errorf("the server sent Hostbay to %s (%s), which it did not follow: Hostbay connects to one Redis server only, not to a Redis Cluster", f[len(f)-1], msg)}
	case strings.Contains(msg, "NOAUTH") || strings.Contains(msg, "WRONGPASS") || strings.Contains(msg, "invalid password") || strings.Contains(msg, "invalid username-password"):
		return &Error{Category: "auth", Err: err}
	case strings.Contains(msg, "NOPERM"):
		return &Error{Category: "permission", Err: err}
	case strings.HasPrefix(msg, "ERR unknown command") || strings.HasPrefix(msg, "ERR wrong number") || strings.HasPrefix(msg, "ERR syntax"):
		return &Error{Category: "syntax", Err: err}
	}
	return &Error{Category: Category(err), Err: err}
}

func (e *redisEngine) Close() error { return e.c.Close() }

// Children lists the database's keys by SCAN with MATCH and COUNT, never
// KEYS, which blocks the server while it walks every key. A page carries
// TreePage keys with their types; the next page continues the same SCAN.
func (e *redisEngine) Children(ctx context.Context, path []string, offset int) ([]Node, bool, error) {
	if len(path) > 0 {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, CatalogTimeout)
	defer cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	if offset == 0 {
		e.scan = redisScan{}
	} else if offset != e.scan.offset {
		return nil, false, errors.New("the key list changed; reload it")
	}
	for len(e.scan.keys) < TreePage && !e.scan.done {
		keys, cur, err := e.c.Scan(ctx, e.scan.cursor, "*", TreePage).Result()
		if err != nil {
			return nil, false, redisError(err)
		}
		e.scan.keys, e.scan.cursor, e.scan.done = append(e.scan.keys, keys...), cur, cur == 0
	}
	n := min(TreePage, len(e.scan.keys))
	page := e.scan.keys[:n]
	e.scan.keys, e.scan.offset = e.scan.keys[n:], offset+n
	pipe := e.c.Pipeline()
	types := make([]*redis.StatusCmd, n)
	for i, k := range page {
		types[i] = pipe.Type(ctx, k)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, false, redisError(err)
	}
	nodes := make([]Node, n)
	for i, k := range page {
		t := types[i].Val()
		e.types[k] = t
		nodes[i] = Node{Kind: "key", Name: k, Path: []string{k}, Detail: t}
	}
	return nodes, len(e.scan.keys) > 0 || !e.scan.done, nil
}

// redisRefused are commands the editor never sends, and why: they stream
// replies that paged results cannot carry, or change the state of the
// pooled connection they happen to run on, which later commands share.
var redisRefused = map[string]string{
	"MONITOR": "streams", "SUBSCRIBE": "streams", "PSUBSCRIBE": "streams", "SSUBSCRIBE": "streams",
	"UNSUBSCRIBE": "streams", "PUNSUBSCRIBE": "streams", "SUNSUBSCRIBE": "streams", "SYNC": "streams", "PSYNC": "streams",
	"SELECT": "connection", "AUTH": "connection", "HELLO": "connection", "RESET": "connection", "QUIT": "connection",
	"MULTI": "connection", "EXEC": "connection", "DISCARD": "connection", "WATCH": "connection", "UNWATCH": "connection",
	"READONLY": "connection", "READWRITE": "connection",
}

// redisTimeoutArg is where each blocking command's timeout argument sits:
// -1 is the last argument.
var redisTimeoutArg = map[string]int{
	"BLPOP": -1, "BRPOP": -1, "BZPOPMIN": -1, "BZPOPMAX": -1, "BRPOPLPUSH": -1, "BLMOVE": -1,
	"BLMPOP": 1, "BZMPOP": 1, "WAIT": -1, "WAITAOF": -1,
}

// checkRedis refuses a command the editor does not send, with why.
func checkRedis(args []string) error {
	name := strings.ToUpper(args[0])
	switch why := redisRefused[name]; {
	case why == "streams":
		return &Error{Category: "syntax", Err: fmt.Errorf("%s streams replies, which a query cannot show; use redis-cli for it", name)}
	case why == "connection":
		return &Error{Category: "syntax", Err: fmt.Errorf("%s changes the connection it runs on, which Hostbay shares between requests, so it is not sent; set the database number in the host's settings", name)}
	case name == "CLIENT" && len(args) > 1 && (strings.EqualFold(args[1], "REPLY") || strings.EqualFold(args[1], "TRACKING")):
		return &Error{Category: "syntax", Err: fmt.Errorf("CLIENT %s changes the shared connection, so it is not sent", strings.ToUpper(args[1]))}
	}
	block := false
	if i, ok := redisTimeoutArg[name]; ok {
		if i < 0 {
			i += len(args)
		}
		block = i > 0 && i < len(args) && zeroTimeout(args[i])
	}
	if name == "XREAD" || name == "XREADGROUP" {
		for i := 1; i+1 < len(args); i++ {
			block = block || (strings.EqualFold(args[i], "BLOCK") && zeroTimeout(args[i+1]))
		}
	}
	if block {
		return &Error{Category: "syntax", Err: fmt.Errorf("%s with a timeout of 0 waits forever; give it a timeout in seconds", name)}
	}
	return nil
}

func zeroTimeout(s string) bool {
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == 0
}

// redisReadOnly are commands that change nothing, so a request of only
// these is safe to run again after its results expired.
var redisReadOnly = map[string]bool{
	"GET": true, "MGET": true, "STRLEN": true, "GETRANGE": true, "EXISTS": true, "TYPE": true, "TTL": true, "PTTL": true,
	"HGET": true, "HGETALL": true, "HKEYS": true, "HVALS": true, "HLEN": true, "HMGET": true, "HEXISTS": true,
	"LRANGE": true, "LLEN": true, "LINDEX": true, "SMEMBERS": true, "SCARD": true, "SISMEMBER": true,
	"ZRANGE": true, "ZCARD": true, "ZSCORE": true, "ZRANK": true, "XRANGE": true, "XLEN": true,
	"SCAN": true, "HSCAN": true, "SSCAN": true, "ZSCAN": true, "DBSIZE": true, "INFO": true, "PING": true,
}

// Query runs each line of text as one command, split into arguments in Go
// the way redis-cli splits them, as its own result set. Every line is
// checked before any is sent. A key's whole value (HGETALL, SMEMBERS,
// LRANGE key 0 -1, ZRANGE key 0 -1 WITHSCORES, XRANGE key - +) asked for
// last is fetched in pages as the results are read.
func (e *redisEngine) Query(ctx context.Context, text string) (Results, error) {
	var cmds [][]string
	rerun := true
	for line := range strings.Lines(text) {
		args, err := splitRedis(line)
		if err != nil {
			return nil, &Error{Category: "syntax", Err: err}
		}
		if len(args) == 0 {
			continue
		}
		if err := checkRedis(args); err != nil {
			return nil, err
		}
		rerun = rerun && redisReadOnly[strings.ToUpper(args[0])]
		cmds = append(cmds, args)
	}
	if len(cmds) == 0 {
		return nil, &Error{Category: "syntax", Err: errors.New("type a Redis command")}
	}
	res := &memResults{rerun: rerun}
	for i, args := range cmds {
		// Only the last command may page: a paged read runs as its rows are
		// read, so an earlier one would see what later commands did.
		if i == len(cmds)-1 {
			if set, ok := e.paged(ctx, args); ok {
				res.sets = append(res.sets, set)
				continue
			}
		}
		v, err := e.do(ctx, args)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(args[0], "GET") && len(args) == 2 {
			// A string key's row, named for the row editor as a key's view.
			res.sets = append(res.sets, resultSet{cols: []Column{{Name: "key", Type: "string"}, {Name: "value", Type: "string"}},
				next: rowList([][]any{{args[1], redisCell(v)}})})
			continue
		}
		res.sets = append(res.sets, redisReply(v))
	}
	return res, nil
}

// do sends one command. A cancelled request returns at once; the command
// itself, which no Redis command lets a client stop, finishes on its own.
func (e *redisEngine) do(ctx context.Context, args []string) (any, error) {
	in := make([]any, len(args))
	for i, a := range args {
		in[i] = a
	}
	type reply struct {
		v   any
		err error
	}
	done := make(chan reply, 1)
	go func() {
		v, err := e.c.Do(context.WithoutCancel(ctx), in...).Result()
		done <- reply{v, err}
	}()
	select {
	case r := <-done:
		if errors.Is(r.err, redis.Nil) {
			return nil, nil
		}
		return r.v, redisError(r.err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// redisReply is a reply as a result set: a single value as one row, an
// array as a row per element, nested arrays as JSON text.
func redisReply(v any) resultSet {
	cols := []Column{{Name: "reply", Type: "reply"}}
	list, ok := v.([]any)
	if !ok {
		return resultSet{cols: cols, next: rowList([][]any{{redisCell(v)}})}
	}
	rows := make([][]any, len(list))
	for i, x := range list {
		rows[i] = []any{redisCell(x)}
	}
	return resultSet{cols: cols, next: rowList(rows)}
}

// redisCell is one reply value as a cell: bytes that are not text show as
// binary.
func redisCell(v any) any {
	switch x := v.(type) {
	case string:
		if !utf8.ValidString(x) {
			return []byte(x)
		}
		return x
	case []any:
		b, _ := json.Marshal(x)
		return string(b)
	case nil, int64:
		return x
	}
	return fmt.Sprint(v)
}

// redisPage is how many elements a paged value fetches at a time.
const redisPage = PageRows

// paged returns a result set that reads a key's whole value a page at a
// time, for the commands that show one, so a large value never sits in
// memory whole. The columns name what the row editor changes.
func (e *redisEngine) paged(ctx context.Context, args []string) (resultSet, bool) {
	name := strings.ToUpper(args[0])
	whole := func(n int, tail ...string) bool {
		if len(args) != n+len(tail) || args[2] != "0" || args[3] != "-1" {
			return false
		}
		for i, t := range tail {
			if !strings.EqualFold(args[n+i], t) {
				return false
			}
		}
		return true
	}
	switch {
	case name == "HGETALL" && len(args) == 2:
		return e.scanned(ctx, []Column{{Name: "field", Type: "hash"}, {Name: "value", Type: "string"}}, func(cur uint64) ([]string, uint64, error) {
			return e.c.HScan(ctx, args[1], cur, "*", redisPage).Result()
		}, 2), true
	case name == "SMEMBERS" && len(args) == 2:
		return e.scanned(ctx, []Column{{Name: "member", Type: "set"}}, func(cur uint64) ([]string, uint64, error) {
			return e.c.SScan(ctx, args[1], cur, "*", redisPage).Result()
		}, 1), true
	case name == "LRANGE" && whole(4):
		key := args[1]
		return e.ranged(ctx, []Column{{Name: "index", Type: "integer"}, {Name: "value", Type: "string"}}, func(i int64) ([][]any, error) {
			vals, err := e.c.LRange(ctx, key, i, i+redisPage-1).Result()
			rows := make([][]any, len(vals))
			for j, v := range vals {
				rows[j] = []any{i + int64(j), redisCell(v)}
			}
			return rows, err
		}), true
	case name == "ZRANGE" && whole(4, "WITHSCORES"):
		key := args[1]
		return e.ranged(ctx, []Column{{Name: "member", Type: "zset"}, {Name: "score", Type: "double"}}, func(i int64) ([][]any, error) {
			vals, err := e.c.ZRangeWithScores(ctx, key, i, i+redisPage-1).Result()
			rows := make([][]any, len(vals))
			for j, v := range vals {
				rows[j] = []any{redisCell(v.Member), strconv.FormatFloat(v.Score, 'g', -1, 64)}
			}
			return rows, err
		}), true
	case name == "XRANGE" && len(args) == 4 && args[2] == "-" && args[3] == "+":
		// Each page starts after the last ID read; an entry stays a row.
		key, start := args[1], "-"
		return e.ranged(ctx, []Column{{Name: "reply", Type: "reply"}}, func(int64) ([][]any, error) {
			v, err := e.c.Do(ctx, "XRANGE", key, start, "+", "COUNT", redisPage).Result()
			list, _ := v.([]any)
			rows := make([][]any, len(list))
			for j, x := range list {
				rows[j] = []any{redisCell(x)}
				if entry, ok := x.([]any); ok && len(entry) > 0 {
					start = afterStreamID(fmt.Sprint(entry[0]))
				}
			}
			return rows, err
		}), true
	}
	return resultSet{}, false
}

// afterStreamID is the smallest stream ID after id, as an inclusive XRANGE
// start: the exclusive "(id" form needs Redis 6.2.
func afterStreamID(id string) string {
	ms, seq, _ := strings.Cut(id, "-")
	n, err := strconv.ParseUint(seq, 10, 64)
	m, err2 := strconv.ParseUint(ms, 10, 64)
	switch {
	case err != nil || err2 != nil:
		return "(" + id
	case n == math.MaxUint64:
		return strconv.FormatUint(m+1, 10) + "-0"
	}
	return ms + "-" + strconv.FormatUint(n+1, 10)
}

// scanned serves a *SCAN over a key, width values to a row.
func (e *redisEngine) scanned(ctx context.Context, cols []Column, scan func(uint64) ([]string, uint64, error), width int) resultSet {
	var buf []string
	var cur uint64
	started := false
	return resultSet{cols: cols, next: func() ([]any, bool, error) {
		for len(buf) < width && (!started || cur != 0) {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			vals, next, err := scan(cur)
			if err != nil {
				return nil, false, redisError(err)
			}
			buf, cur, started = append(buf, vals...), next, true
		}
		if len(buf) < width {
			return nil, false, nil
		}
		row := make([]any, width)
		for i := range row {
			row[i] = redisCell(buf[i])
		}
		buf = buf[width:]
		return row, true, nil
	}}
}

// ranged serves an index range a page at a time until a short page.
func (e *redisEngine) ranged(ctx context.Context, cols []Column, page func(int64) ([][]any, error)) resultSet {
	var buf [][]any
	var at int64
	end := false
	return resultSet{cols: cols, next: func() ([]any, bool, error) {
		if len(buf) == 0 && !end {
			rows, err := page(at)
			if err != nil {
				return nil, false, redisError(err)
			}
			buf, at, end = rows, at+int64(len(rows)), len(rows) < redisPage
		}
		if len(buf) == 0 {
			return nil, false, nil
		}
		r := buf[0]
		buf = buf[1:]
		return r, true, nil
	}}
}

// redisKeyColumn is the column naming a row of each type's value, which
// the row editor sends back as the row's key.
var redisKeyColumn = map[string]string{"string": "key", "hash": "field", "list": "index", "set": "member", "zset": "member"}

// redisValueColumn is the one column of each type's value EditRow changes.
var redisValueColumn = map[string]string{"string": "value", "hash": "value", "list": "value", "set": "member", "zset": "score"}

// redisShow is the command that shows a key of each type.
var redisShow = map[string]string{"string": "GET", "hash": "HGETALL", "list": "LRANGE", "set": "SMEMBERS", "zset": "ZRANGE"}

// EditableTable opens a key: its type says how its rows are named. Other
// types, such as streams, open read-only.
func (e *redisEngine) EditableTable(ctx context.Context, path []string) (*Table, error) {
	if len(path) != 1 {
		return nil, errors.New("that is not a key")
	}
	ctx, cancel := context.WithTimeout(ctx, CatalogTimeout)
	defer cancel()
	t, err := e.c.Type(ctx, path[0]).Result()
	if err != nil {
		return nil, redisError(err)
	}
	if t == "none" {
		return nil, errors.New("this key no longer exists; reload the key list")
	}
	e.mu.Lock()
	e.types[path[0]] = t
	e.mu.Unlock()
	if col, ok := redisKeyColumn[t]; ok {
		return &Table{Path: path, Key: []string{col}, Edit: []string{redisValueColumn[t]}, NoNull: true}, nil
	}
	return nil, nil
}

// SelectTable is the command that shows the key's value, by the type the
// tree or EditableTable saw.
func (e *redisEngine) SelectTable(path []string) string {
	key := quoteRedis(path[0])
	e.mu.Lock()
	t := e.types[path[0]]
	e.mu.Unlock()
	switch t {
	case "list":
		return "LRANGE " + key + " 0 -1"
	case "zset":
		return "ZRANGE " + key + " 0 -1 WITHSCORES"
	case "stream":
		return "XRANGE " + key + " - +"
	}
	if cmd, ok := redisShow[t]; ok {
		return cmd + " " + key
	}
	return "TYPE " + key
}

// EditRow changes one value of a key by its type: a string's value, a
// hash field's value, a list element by index, a set member, or a
// sorted-set member's score. It runs under WATCH, checking the key still
// has that type and the row still exists, so nothing is saved, and
// nothing recreated, when the key changed meanwhile.
func (e *redisEngine) EditRow(ctx context.Context, edit RowEdit) error {
	if len(edit.Path) != 1 {
		return errors.New("that is not a key")
	}
	key := edit.Path[0]
	one := func(m map[string]*string, col string) (string, error) {
		v, ok := m[col]
		if !ok || len(m) != 1 {
			return "", fmt.Errorf("only the %s can be changed here", col)
		}
		if v == nil {
			return "", errors.New("Redis has no NULL; enter a value")
		}
		return *v, nil
	}
	changed := errors.New("the key changed or was deleted meanwhile, so nothing was saved; reload it")
	err := e.c.Watch(ctx, func(tx *redis.Tx) error {
		t, err := tx.Type(ctx, key).Result()
		if err != nil {
			return err
		}
		col, ok := redisKeyColumn[t]
		if !ok {
			if t == "none" {
				return changed
			}
			return fmt.Errorf("a %s cannot be edited here", t)
		}
		row, err := one(edit.Key, col)
		if err != nil {
			return errors.New("the row's key does not match the key's type; reload it")
		}
		v, err := one(edit.Set, redisValueColumn[t])
		if err != nil {
			return err
		}
		// exists reports whether the row is still there; it is checked
		// under WATCH, so a change after it fails the transaction.
		var exists bool
		var apply func(redis.Pipeliner)
		switch t {
		case "string":
			exists = row == key
			apply = func(p redis.Pipeliner) { p.SetArgs(ctx, key, v, redis.SetArgs{KeepTTL: true}) }
		case "hash":
			exists, err = tx.HExists(ctx, key, row).Result()
			apply = func(p redis.Pipeliner) { p.HSet(ctx, key, row, v) }
		case "list":
			i, perr := strconv.ParseInt(row, 10, 64)
			if perr != nil {
				return errors.New("the row's index is not a number")
			}
			var n int64
			n, err = tx.LLen(ctx, key).Result()
			exists = i >= 0 && i < n
			apply = func(p redis.Pipeliner) { p.LSet(ctx, key, i, v) }
		case "set":
			exists, err = tx.SIsMember(ctx, key, row).Result()
			if err == nil && exists && v != row {
				if dup, derr := tx.SIsMember(ctx, key, v).Result(); derr != nil || dup {
					return errors.Join(derr, errors.New("that member is already in the set"))
				}
			}
			apply = func(p redis.Pipeliner) { p.SRem(ctx, key, row); p.SAdd(ctx, key, v) }
		case "zset":
			score, perr := strconv.ParseFloat(v, 64)
			if perr != nil {
				return errors.New("the score must be a number")
			}
			_, err = tx.ZScore(ctx, key, row).Result()
			exists = err == nil
			if errors.Is(err, redis.Nil) {
				err = nil
			}
			apply = func(p redis.Pipeliner) { p.ZAddXX(ctx, key, redis.Z{Score: score, Member: row}) }
		}
		if err != nil {
			return err
		}
		if !exists {
			return changed
		}
		_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error { apply(p); return nil })
		return err
	}, key)
	if errors.Is(err, redis.TxFailedErr) {
		return changed
	}
	return redisError(err)
}

// splitRedis splits a command line into arguments as redis-cli does: bare
// words, "double quotes" with \n, \r, \t, \b, \a, \\, \" and \xHH escapes,
// and 'single quotes' with only \' escaped. A quote must be followed by a
// space or the end.
func splitRedis(line string) ([]string, error) {
	var args []string
	s := strings.TrimRight(line, "\r\n")
	for i := 0; ; {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i == len(s) {
			return args, nil
		}
		var b strings.Builder
		switch q := s[i]; q {
		case '"', '\'':
			i++
			for {
				if i >= len(s) {
					return nil, errors.New("a quote is not closed")
				}
				c := s[i]
				switch {
				case c == q:
					i++
					if i < len(s) && s[i] != ' ' && s[i] != '\t' {
						return nil, errors.New("a closing quote must be followed by a space")
					}
					goto next
				case c == '\\' && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
					b.WriteByte('\'')
					i += 2
				case c == '\\' && q == '"' && i+3 < len(s) && s[i+1] == 'x' && isHex(s[i+2]) && isHex(s[i+3]):
					n, _ := strconv.ParseUint(s[i+2:i+4], 16, 8)
					b.WriteByte(byte(n))
					i += 4
				case c == '\\' && q == '"' && i+1 < len(s):
					b.WriteByte(unescape(s[i+1]))
					i += 2
				default:
					b.WriteByte(c)
					i++
				}
			}
		default:
			for i < len(s) && s[i] != ' ' && s[i] != '\t' {
				b.WriteByte(s[i])
				i++
			}
		}
	next:
		args = append(args, b.String())
	}
}

// unescape is the byte a backslash and c stand for in double quotes.
func unescape(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	case 'b':
		return '\b'
	case 'a':
		return '\a'
	}
	return c
}

func isHex(c byte) bool { return strings.IndexByte("0123456789abcdefABCDEF", c) >= 0 }

// quoteRedis writes s so splitRedis reads it back unchanged.
func quoteRedis(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool { return r <= ' ' || r == '"' || r == '\'' || r == '\\' || r >= 0x7f }) < 0 {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < ' ' || c >= 0x7f:
			fmt.Fprintf(&b, "\\x%02x", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
