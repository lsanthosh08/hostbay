package ai

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"hostbay/internal/ai/aitest"
)

// The question goes with the dialect and the schema's names; the SQL in
// the reply's code block comes back.
func TestSQLSendsSchemaNamesAndReturnsTheStatement(t *testing.T) {
	srv, p := fakeProvider(t)
	srv.Push(aitest.Reply{Body: aitest.AnthropicText("Here it is:\n```sql\nSELECT name FROM public.users\nWHERE active;\n```\nDone.")})
	got, err := SQL(context.Background(), p, "m", SQLInput{
		Dialect:  "postgres",
		Tables:   []SQLTable{{Name: "public.users", Columns: []string{"id", "name", "active"}}},
		Question: "names of active users",
	})
	if err != nil || got != "SELECT name FROM public.users\nWHERE active;" {
		t.Fatalf("SQL = %q, %v", got, err)
	}
	body := srv.Requests()[0].Body
	for _, want := range []string{"postgres", "public.users", "active", "names of active users"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("request lacks %q: %s", want, body)
		}
	}
}

// A reply with no fence is taken whole; an empty one and a provider error
// are errors.
func TestSQLReplyShapes(t *testing.T) {
	srv, p := fakeProvider(t)
	srv.Push(aitest.Reply{Body: aitest.AnthropicText("  SELECT 1;  ")}, aitest.Reply{Body: aitest.AnthropicText("   ")}, aitest.Reply{Status: 529, Body: aitest.Overloaded})
	in := SQLInput{Dialect: "sqlite", Question: "one"}
	if got, err := SQL(context.Background(), p, "m", in); err != nil || got != "SELECT 1;" {
		t.Fatalf("plain reply: %q, %v", got, err)
	}
	if _, err := SQL(context.Background(), p, "m", in); err == nil {
		t.Fatal("an empty reply was taken as SQL")
	}
	if _, err := SQL(context.Background(), p, "m", in); err == nil {
		t.Fatal("a provider error was not returned")
	}
}

// OpenAI-compatible servers refuse an empty tools list, so a one-shot
// request sends none.
func TestOneShotOpenAISendsNoTools(t *testing.T) {
	srv := aitest.New(t)
	p, err := New(OpenAIChat, srv.URL, "", "Hostbay/test", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	srv.Push(aitest.Reply{Body: "data: {\"choices\":[{\"delta\":{\"content\":\"SELECT 1;\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"})
	if got, err := SQL(context.Background(), p, "m", SQLInput{Dialect: "mysql", Question: "one"}); err != nil || got != "SELECT 1;" {
		t.Fatalf("SQL = %q, %v", got, err)
	}
	if body := srv.Requests()[0].Body; bytes.Contains(body, []byte(`"tools"`)) {
		t.Fatalf("request = %s", body)
	}
}
