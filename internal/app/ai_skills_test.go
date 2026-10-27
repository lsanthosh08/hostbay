package app

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hostbay/internal/ai/aitest"
	"hostbay/internal/database"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
	"hostbay/internal/tlstrust"
)

// The settings screen saves, lists and deletes skills; two cannot share a
// name, the page cannot mark one agent-authored, and a bad body is refused.
func TestAISkillsCRUD(t *testing.T) {
	e := newAIService(t)
	sk, err := e.s.SaveSkill(AISkill{Name: "deploy-check", Kind: store.SkillKind, Body: "<script>alert(1)</script>", AgentAuthored: true})
	if err != nil || sk.ID == "" || sk.AgentAuthored || sk.Body != "<script>alert(1)</script>" {
		t.Fatalf("saved %+v, %v", sk, err)
	}
	if _, err := e.s.SaveSkill(AISkill{Name: "deploy-check", Kind: store.TemplateKind, Body: "x"}); err == nil {
		t.Fatal("a second skill took the same name")
	}
	if _, err := e.s.SaveSkill(AISkill{Name: "bad", Kind: store.SkillKind, Body: "a\x1b[2Jb"}); err == nil {
		t.Fatal("a body with an escape sequence was saved")
	}
	sk.Body = "edited"
	if got, err := e.s.SaveSkill(sk); err != nil || got.ID != sk.ID || got.Body != "edited" {
		t.Fatalf("edit = %+v, %v", got, err)
	}
	l, _ := e.s.ListSkills()
	if len(l) != 1 || l[0].Body != "edited" {
		t.Fatalf("list = %+v", l)
	}
	if err := e.s.DeleteSkill(sk.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := e.s.ListSkills(); len(l) != 0 {
		t.Fatalf("after delete: %+v", l)
	}
}

// An approved save_skill replaces the skill of that name and is marked
// agent-authored.
func TestAIAgentSavedSkillReplacesByName(t *testing.T) {
	e := newAIService(t)
	old, err := e.s.SaveSkill(AISkill{Name: "deploy-check", Kind: store.TemplateKind, Body: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.agent.SaveSkill(store.Skill{Name: "deploy-check", Kind: store.SkillKind, Body: "new", AgentAuthored: true}); err != nil {
		t.Fatal(err)
	}
	l, _ := e.s.ListSkills()
	if len(l) != 1 || l[0].ID != old.ID || l[0].Body != "new" || !l[0].AgentAuthored || l[0].Kind != store.SkillKind {
		t.Fatalf("skills = %+v", l)
	}
	if s := e.s.agent.Skills("deploy-check"); s == nil || s.Body != "new" {
		t.Fatalf("lookup = %+v", s)
	}
}

// suggestEnv is an AI service over a saved SSH host's open terminal.
func suggestEnv(t *testing.T) (*AIService, *toolsEnv, string, *aitest.Server) {
	t.Helper()
	e := newToolsEnv(t, fakeVerifier(secrets.Passed))
	s := NewAIService(e.c.creds.st, e.c.creds.vault, e.c.creds, e.c, tlstrust.New(t.TempDir()))
	s.emit = func(string, any) {}
	id, sh := e.open(t)
	// An SSH shell not probed yet: a suggestion must not run the probe.
	e.c.track(id, ptyMeta{session: "unprobed", hostID: e.hostID, kind: "ssh"})
	sh.say("SCREEN-OUTPUT-SENTINEL\r\n$ ")
	srv := aitest.New(t)
	if _, err := s.SaveProvider(AIProvider{Name: "Claude", Protocol: "anthropic", BaseURL: srv.URL, Model: "claude-haiku-5-5"}, sampleAIKey); err != nil {
		t.Fatal(err)
	}
	return s, e, id, srv
}

// AI suggestions are off on a fresh install: nothing is sent.
func TestAISuggestionsAreOffByDefault(t *testing.T) {
	s, _, id, srv := suggestEnv(t)
	if _, err := s.Suggest(context.Background(), id, "systemctl st"); err == nil {
		t.Fatal("a suggestion was requested while they are off")
	}
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("%d requests sent", n)
	}
}

// When on, the request holds the current line, the host's kind and OS and
// its last 20 history entries, scrubbed of the host's password, and no
// screen output. A terminal where Go adds local echo sends nothing.
func TestAISuggestionSendsLineKindOSAndTwentyEntries(t *testing.T) {
	s, e, id, srv := suggestEnv(t)
	if err := s.st.EditLocal(func(l *store.Local) error { on := true; l.AISuggestions = &on; return nil }); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 25; i++ {
		line := fmt.Sprintf("echo entry-%02d", i)
		if i == 25 {
			line = "mysql -p" + savedPassword
		}
		if err := s.st.AddHistory(e.hostID, line); err != nil {
			t.Fatal(err)
		}
	}
	srv.Push(aitest.Reply{Body: aitest.AnthropicText("systemctl status nginx")})
	got, err := s.Suggest(context.Background(), id, "systemctl st")
	if err != nil || got != "systemctl status nginx" {
		t.Fatalf("Suggest = %q, %v", got, err)
	}
	body := srv.Requests()[0].Body
	for _, want := range []string{"systemctl st", "ssh", "Operating system:", "echo entry-06", "echo entry-24", "mysql -p"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("request lacks %q", want)
		}
	}
	for _, leak := range []string{"echo entry-05", "echo entry-01", savedPassword, "SCREEN-OUTPUT-SENTINEL", sampleAIKey} {
		if bytes.Contains(body, []byte(leak)) {
			t.Errorf("request carries %q", leak)
		}
	}

	// A line holding a saved secret is never sent.
	if got, err := s.Suggest(context.Background(), id, "sshpass -p "+savedPassword); err != nil || got != "" {
		t.Fatalf("secret line: %q, %v", got, err)
	}
	e.c.tools.get(id).echo = true
	if _, err := s.Suggest(context.Background(), id, "ls"); err == nil {
		t.Fatal("a local-echo terminal asked for a suggestion")
	}
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

// fakeSQL is a SQL database with many tables, counting the requests the
// user runs.
type fakeSQL struct {
	tables  int
	queries atomic.Int32
}

func (f *fakeSQL) Children(_ context.Context, path []string, offset int) ([]database.Node, bool, error) {
	if len(path) == 1 {
		return []database.Node{{Kind: "column", Name: "id", Path: append(path, "id")}, {Kind: "column", Name: "api_token", Path: append(path, "api_token")}}, false, nil
	}
	var out []database.Node
	for i := offset; i < f.tables && len(out) < database.TreePage; i++ {
		n := fmt.Sprintf("t%03d", i)
		out = append(out, database.Node{Kind: "table", Name: n, Path: []string{n}, HasChildren: true})
	}
	return out, offset+len(out) < f.tables, nil
}
func (f *fakeSQL) Query(context.Context, string) (database.Results, error) {
	f.queries.Add(1)
	return nil, fmt.Errorf("not served")
}
func (f *fakeSQL) Close() error { return nil }

func sqlEnv(t *testing.T, e database.Engine, kind string) (*aiEnv, *DatabaseService, *aitest.Server) {
	t.Helper()
	a := newAIService(t)
	d, _, _, _ := newDatabaseService(t)
	LinkAIDatabase(a.s, d)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d.dbs["db1"] = &dbSession{engine: e, kind: kind, ctx: ctx, cancel: cancel}
	srv := aitest.New(t)
	if _, err := a.s.SaveProvider(AIProvider{Name: "Claude", Protocol: "anthropic", BaseURL: srv.URL, Model: "claude-opus-5-5"}, sampleAIKey); err != nil {
		t.Fatal(err)
	}
	return a, d, srv
}

// A question in a PostgreSQL tab comes back as SQL for the editor; the
// database sees no request, in bypass too, until the user presses Run.
// The schema sent is cut to 200 tables.
func TestAIAskSQLRunsNothingAndCapsTheSchema(t *testing.T) {
	f := &fakeSQL{tables: 500}
	a, _, srv := sqlEnv(t, f, "postgres")
	a.s.SetBypass(context.Background(), true)
	srv.Push(aitest.Reply{Body: aitest.AnthropicText("```sql\nDELETE FROM t001;\n```")})
	got, err := a.s.AskSQL(context.Background(), "db1", "remove everything from t001")
	if err != nil || got != "DELETE FROM t001;" {
		t.Fatalf("AskSQL = %q, %v", got, err)
	}
	if n := f.queries.Load(); n != 0 {
		t.Fatalf("the database saw %d requests", n)
	}
	body := string(srv.Requests()[0].Body)
	if !strings.Contains(body, "t199: id, api_token") || strings.Contains(body, "t200") || !strings.Contains(body, "postgres") {
		t.Fatalf("schema sent: %s", body)
	}
}

// With a table whose rows hold a secret, the provider gets table and
// column names and no row value.
func TestAIAskSQLSendsNamesNotRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE accounts (id INTEGER PRIMARY KEY, owner TEXT, api_token TEXT);
		INSERT INTO accounts (owner, api_token) VALUES ('alice', 'ROW-SECRET-token-123')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e, err := database.OpenSQLite(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	a, _, srv := sqlEnv(t, e, "sqlite")
	srv.Push(aitest.Reply{Body: aitest.AnthropicText("SELECT owner FROM accounts;")})
	if _, err := a.s.AskSQL(context.Background(), "db1", "who owns accounts?"); err != nil {
		t.Fatal(err)
	}
	body := string(srv.Requests()[0].Body)
	if !strings.Contains(body, "accounts: id, owner, api_token") || strings.Contains(body, "ROW-SECRET") || strings.Contains(body, "alice") {
		t.Fatalf("request = %s", body)
	}
}

// A provider error comes back as the error, with no SQL; a NoSQL tab
// cannot ask.
func TestAIAskSQLErrors(t *testing.T) {
	a, d, srv := sqlEnv(t, &fakeSQL{tables: 1}, "postgres")
	srv.Push(aitest.Reply{Status: 529, Body: aitest.Overloaded})
	if got, err := a.s.AskSQL(context.Background(), "db1", "anything"); err == nil || got != "" {
		t.Fatalf("provider error: %q, %v", got, err)
	}
	srv.Push(aitest.Reply{Body: aitest.AnthropicText("SELECT 1;\r\nSELECT 2;")})
	if got, err := a.s.AskSQL(context.Background(), "db1", "two"); err != nil || got != "SELECT 1;\nSELECT 2;" {
		t.Fatalf("CRLF reply: %q, %v", got, err)
	}
	d.dbs["db2"] = &dbSession{engine: &fakeSQL{}, kind: "redis", ctx: context.Background(), cancel: func() {}}
	if _, err := a.s.AskSQL(context.Background(), "db2", "anything"); err == nil {
		t.Fatal("a Redis tab asked for SQL")
	}
}

// slowSQL answers the table list at once and each table's columns only
// after the walk's deadline has passed for every table past the third.
type slowSQL struct{ fakeSQL }

func (f *slowSQL) Children(ctx context.Context, path []string, offset int) ([]database.Node, bool, error) {
	if len(path) == 1 && path[0] >= "t003" {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	return f.fakeSQL.Children(ctx, path, offset)
}

// A catalog too slow to read in time still sends the tables read so far.
func TestAIAskSQLSendsWhatTheDeadlineAllowed(t *testing.T) {
	old := schemaTimeout
	schemaTimeout = 200 * time.Millisecond
	t.Cleanup(func() { schemaTimeout = old })
	a, _, srv := sqlEnv(t, &slowSQL{fakeSQL{tables: 10}}, "postgres")
	srv.Push(aitest.Reply{Body: aitest.AnthropicText("SELECT 1;")})
	if _, err := a.s.AskSQL(context.Background(), "db1", "q"); err != nil {
		t.Fatal(err)
	}
	body := string(srv.Requests()[0].Body)
	if !strings.Contains(body, "t002: id, api_token") || strings.Contains(body, "t003") {
		t.Fatalf("schema sent: %s", body)
	}
}
