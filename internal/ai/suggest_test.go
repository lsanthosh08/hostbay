package ai

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"hostbay/internal/ai/aitest"
)

func fakeProvider(t *testing.T) (*aitest.Server, Provider) {
	srv := aitest.New(t)
	p, err := New(Anthropic, srv.URL, sampleKey, "Hostbay/test", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	return srv, p
}

// One request with no tools carries the line, the host's kind and OS and
// the history it is given; a reply that extends the line comes back.
func TestSuggestSendsTheLineAndHistoryOnly(t *testing.T) {
	srv, p := fakeProvider(t)
	srv.Push(aitest.Reply{Body: aitest.AnthropicText("```\ntail -f /var/log/nginx/error.log\n```")})
	got, err := Suggest(context.Background(), p, "claude-haiku-5-5", SuggestInput{
		Line: "tail -f /var/log/ng", Kind: "ssh", OS: "unix-like", History: []string{"cd /srv", "systemctl status nginx"},
	})
	if err != nil || got != "tail -f /var/log/nginx/error.log" {
		t.Fatalf("Suggest = %q, %v", got, err)
	}
	r := srv.Requests()
	if len(r) != 1 {
		t.Fatalf("%d requests", len(r))
	}
	for _, want := range []string{"tail -f /var/log/ng", "ssh", "unix-like", "cd /srv", "systemctl status nginx"} {
		if !bytes.Contains(r[0].Body, []byte(want)) {
			t.Errorf("request lacks %q: %s", want, r[0].Body)
		}
	}
	if bytes.Contains(r[0].Body, []byte(`"tools"`)) {
		t.Errorf("a completion request offers tools: %s", r[0].Body)
	}
}

// Accepting a suggestion types it, so a reply that would press Enter,
// carry an escape sequence or not extend the line gives nothing.
func TestSuggestRefusesWhatCouldRunOrMislead(t *testing.T) {
	for _, reply := range []string{
		"ls -la\rrm -rf ~",
		"ls -la\nrm -rf ~", // only the first line is taken, but it must still extend the line
		"\x1b[31mls -la",
		"ls -la ‮~ fr- mr",
		"cd /tmp",
		"ls",
	} {
		srv, p := fakeProvider(t)
		srv.Push(aitest.Reply{Body: aitest.AnthropicText(reply)})
		got, err := Suggest(context.Background(), p, "m", SuggestInput{Line: "ls -l", Kind: "ssh", OS: "unix-like"})
		if err != nil || (got != "" && got != "ls -la") {
			t.Errorf("reply %q: Suggest = %q, %v", reply, got, err)
		}
		if reply == "ls -la\nrm -rf ~" && got != "ls -la" {
			t.Errorf("first line of a multi-line reply: %q", got)
		}
		if reply != "ls -la\nrm -rf ~" && got != "" {
			t.Errorf("reply %q gave %q", reply, got)
		}
	}
}
