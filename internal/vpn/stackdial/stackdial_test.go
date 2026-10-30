package stackdial

import (
	"slices"
	"testing"
)

func TestSearchNames(t *testing.T) {
	search := []string{"corp.example", "lab.example"}
	for _, c := range []struct {
		name string
		want []string
	}{
		{"db1", []string{"db1.corp.example", "db1.lab.example", "db1"}},
		{"db1.eu", []string{"db1.eu", "db1.eu.corp.example", "db1.eu.lab.example"}},
		{"db1.corp.example.", []string{"db1.corp.example"}}, // netstack adds the root dot itself
	} {
		if got := searchNames(c.name, search); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	if got := searchNames("db1", nil); !slices.Equal(got, []string{"db1"}) {
		t.Errorf("no search list: got %q", got)
	}
}
