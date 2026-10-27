package app

import "testing"

func TestInfoReportsNameAndVersion(t *testing.T) {
	old := Version
	Version = "1.2.3"
	defer func() { Version = old }()

	got := New().Info()
	if got.Name != "Hostbay" || got.Version != "1.2.3" {
		t.Fatalf("Info() = %+v, want Hostbay 1.2.3", got)
	}
}
