package admin

import "testing"

// hostile are inputs that would run a second command, split an argument or
// pass an option if they reached a shell unchecked.
var hostile = []string{
	"", "a b", "a'b", `a"b`, "a;rm -rf /", "a$(id)", "a`id`", "a\nid", "a|id", "a&id",
	"a>x", "a\\b", "-rf", "--help",
}

// Ported from docker.rs validate_token_rejects_metachars.
func TestValidToken(t *testing.T) {
	ok := []struct{ s, extra string }{
		{"nginx:1.25", "_-./:"}, {"abc123", ""}, {"ghcr.io/org/app@sha256:ab12", "_./:@-"},
	}
	for _, c := range ok {
		if err := ValidToken(c.s, c.extra); err != nil {
			t.Errorf("ValidToken(%q, %q) = %v, want ok", c.s, c.extra, err)
		}
	}
	bad := []struct{ s, extra string }{
		{"", "_-"}, {"a;rm -rf", "_-./:"}, {"a$(x)", "_-./:"}, {"a b", "_-./:"},
	}
	for _, c := range bad {
		if ValidToken(c.s, c.extra) == nil {
			t.Errorf("ValidToken(%q, %q) = nil, want error", c.s, c.extra)
		}
	}
	for _, s := range hostile {
		if ValidToken(s, "_-./:@") == nil {
			t.Errorf("ValidToken(%q) = nil, want error", s)
		}
	}
}

func TestValidUnit(t *testing.T) {
	for _, s := range []string{"nginx.service", "getty@tty1.service", "sys-devices-a:b.device", "docker"} {
		if err := validUnit(s); err != nil {
			t.Errorf("validUnit(%q) = %v", s, err)
		}
	}
	for _, s := range append(hostile, "nginx;reboot", "a=b", "-Hevil") {
		if validUnit(s) == nil {
			t.Errorf("validUnit(%q) = nil, want error", s)
		}
	}
}

func TestValidIisName(t *testing.T) {
	for _, s := range []string{"Default Web Site", "api.example_1-test", "DefaultAppPool"} {
		if err := validIisName(s); err != nil {
			t.Errorf("validIisName(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "a'b", `a"b`, "a;Stop-Computer", "a$(x)", "a`b", "a\nb", "a|b", "a&b", "a{b}"} {
		if validIisName(s) == nil {
			t.Errorf("validIisName(%q) = nil, want error", s)
		}
	}
}

func TestValidComposePath(t *testing.T) {
	for _, s := range []string{"/srv/app/docker-compose.yml", "/home/me/my app/compose.yaml", "~/x"} {
		if err := ValidComposePath(s); err != nil {
			t.Errorf("ValidComposePath(%q) = %v", s, err)
		}
	}
	for _, s := range []string{"", "/a'b", `/a"b`, "/a`id`", "-delete", "/a\nb"} {
		if ValidComposePath(s) == nil {
			t.Errorf("ValidComposePath(%q) = nil, want error", s)
		}
	}
}
