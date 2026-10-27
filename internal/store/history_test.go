package store

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestHistoryKeepsTheLastThousandPerHost(t *testing.T) {
	s := openTestStore(t)
	for i := 1; i <= 1001; i++ {
		if err := s.AddHistory("web", "cmd "+strconv.Itoa(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddHistory("db", "psql"); err != nil {
		t.Fatal(err)
	}
	h, err := s.History("web")
	if err != nil || len(h) != 1000 || h[0] != "cmd 2" || h[999] != "cmd 1001" {
		t.Fatalf("history = %d entries from %q, %v", len(h), h[0], err)
	}
	if err := s.ClearHistory("web"); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.History("web"); len(h) != 0 {
		t.Fatalf("cleared history = %q", h)
	}
	if h, _ := s.History("db"); len(h) != 1 {
		t.Fatalf("another host's history = %q", h)
	}
	fi, err := os.Stat(filepath.Join(s.dir, historyName))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	d, _ := s.Data()
	for _, doc := range d.docs() {
		if doc.name == historyName {
			t.Fatal("history is one of the synced and backed-up documents")
		}
	}
}
