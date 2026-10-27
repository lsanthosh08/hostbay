package ai

import (
	"strings"
	"testing"
)

// A host's saved password printed into the output is removed, even when
// colour codes split it; ANSI and control bytes go; long output keeps its
// head and tail.
func TestCleanScrubsStripsAndTruncates(t *testing.T) {
	out := Clean("\x1b[1;32mroot@web\x1b[0m:~# cat pw\r\nhun\x1b[0mter2\r\nok\x1b]0;title\x07\x07done", []string{"hunter2"})
	if strings.Contains(out, "hunter2") || strings.Contains(out, "\x1b") || strings.Contains(out, "\r") || strings.Contains(out, "\x07") {
		t.Fatalf("cleaned = %q", out)
	}
	if out != "root@web:~# cat pw\n[secret removed]\nokdone" {
		t.Fatalf("cleaned = %q", out)
	}
	long := strings.Repeat("a", 10000) + "MIDDLE" + strings.Repeat("é", 10000) + "END"
	c := Clean(long, nil)
	if len(c) > headKeep+tailKeep+64 || strings.Contains(c, "MIDDLE") || !strings.HasPrefix(c, "aaaa") || !strings.HasSuffix(c, "END") || !strings.Contains(c, "bytes not sent") {
		t.Fatalf("truncated to %d bytes", len(c))
	}
	if !strings.Contains(Clean("x", nil), "x") {
		t.Fatal("short output changed")
	}
}
