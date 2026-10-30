package vpnconf

import (
	"strings"
	"testing"
)

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestUnsupportedFile(t *testing.T) {
	if _, err := Parse(fixture("../tunnelhub/vpn-config.json"), ""); err == nil {
		t.Fatal("a .json file is not a VPN configuration")
	}
}
