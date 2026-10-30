package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// thEnv is a TunnelHubService over a fake filesystem in a temp dir, whose
// runner records each command instead of running it. osascript deletes the
// files its script names, as the real admin prompt would.
type thEnv struct {
	s       *TunnelHubService
	calls   [][]string
	dialogs []string
	answer  bool
	cancel  bool // osascript reports the admin prompt cancelled
}

func newTHEnv(t *testing.T, items ...string) *thEnv {
	t.Helper()
	root, home := t.TempDir(), t.TempDir()
	e := &thEnv{answer: true}
	e.s = &TunnelHubService{goos: "darwin", root: root, home: home, uid: 501}
	e.s.confirm = func(_ context.Context, _, msg string) bool { e.dialogs = append(e.dialogs, msg); return e.answer }
	e.s.run = func(name string, args ...string) (int, string, error) {
		e.calls = append(e.calls, append([]string{name}, args...))
		if name == "/usr/bin/osascript" {
			if e.cancel {
				return 1, "execution error: User canceled. (-128)", nil
			}
			for _, sp := range e.s.spots() {
				if sp.Admin && strings.Contains(args[1], "'"+sp.Path+"'") {
					os.Remove(sp.Path)
				}
			}
		}
		return 0, "", nil
	}
	for _, k := range items {
		e.put(t, k)
	}
	return e
}

func (e *thEnv) path(kind string) string {
	for _, sp := range e.s.spots() {
		if sp.Kind == kind {
			return sp.Path
		}
	}
	panic("no spot " + kind)
}

func (e *thEnv) put(t *testing.T, kind string) {
	t.Helper()
	p := e.path(kind)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if kind == "app" {
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *thEnv) exists(kind string) bool {
	_, err := os.Lstat(e.path(kind))
	return err == nil
}

var thAll = []string{"daemon", "helper", "socket", "agent", "config", "app"}

func TestTunnelHubDetectReportsOnlyPresentItems(t *testing.T) {
	for _, kind := range thAll {
		t.Run(kind, func(t *testing.T) {
			e := newTHEnv(t, kind)
			f := e.s.Detect()
			var got []string
			for _, it := range f.Items {
				got = append(got, it.Kind+"="+it.Path)
			}
			if f.ConfigPath != "" {
				got = append(got, "config="+f.ConfigPath)
			}
			if f.AppPath != "" {
				got = append(got, "app="+f.AppPath)
			}
			if want := []string{kind + "=" + e.path(kind)}; !slices.Equal(got, want) {
				t.Fatalf("found %v, want %v", got, want)
			}
		})
	}
	if f := newTHEnv(t).s.Detect(); len(f.Items) != 0 || f.ConfigPath != "" || f.AppPath != "" {
		t.Fatalf("nothing installed, found %+v", f)
	}
}

func TestTunnelHubDetectFindsNothingOffMacOS(t *testing.T) {
	e := newTHEnv(t, thAll...)
	e.s.goos = "linux"
	if f := e.s.Detect(); len(f.Items) != 0 || f.ConfigPath != "" || f.AppPath != "" {
		t.Fatalf("linux found %+v", f)
	}
}

func TestTunnelHubRemoveDeclinedRemovesNothing(t *testing.T) {
	e := newTHEnv(t, thAll...)
	e.answer = false
	r, err := e.s.Remove(context.Background())
	if err != nil || r.Status != TunnelHubDeclined || len(r.Removed) != 0 {
		t.Fatalf("Remove = %+v, %v", r, err)
	}
	if len(e.calls) != 0 {
		t.Fatalf("ran %v after a declined dialog", e.calls)
	}
	for _, k := range thAll {
		if !e.exists(k) {
			t.Fatalf("%s removed after a declined dialog", k)
		}
	}
	msg := e.dialogs[0]
	for _, k := range []string{"daemon", "helper", "socket", "agent"} {
		if !strings.Contains(msg, e.path(k)) {
			t.Errorf("dialog does not list %s: %q", k, msg)
		}
	}
	if !strings.Contains(msg, "only while Hostbay runs") {
		t.Errorf("dialog does not say routes last only while Hostbay runs: %q", msg)
	}
}

func TestTunnelHubRemoveScriptNamesOnlyRootPathsQuoted(t *testing.T) {
	e := newTHEnv(t, thAll...)
	r, err := e.s.Remove(context.Background())
	if err != nil || r.Status != TunnelHubRemoved {
		t.Fatalf("Remove = %+v, %v", r, err)
	}
	var osa []string
	for _, c := range e.calls {
		if c[0] == "/usr/bin/osascript" {
			osa = c
		}
	}
	if len(osa) != 3 || osa[1] != "-e" {
		t.Fatalf("osascript calls: %v", e.calls)
	}
	script := osa[2]
	for _, k := range []string{"daemon", "helper", "socket"} {
		if !strings.Contains(script, "'"+e.path(k)+"'") {
			t.Errorf("script lacks quoted %s: %s", k, script)
		}
	}
	for _, k := range []string{"agent", "config", "app"} {
		if strings.Contains(script, e.path(k)) {
			t.Errorf("script names %s: %s", k, script)
		}
	}
	if !strings.Contains(script, "launchctl bootout system '"+e.path("daemon")+"'") || !strings.HasSuffix(script, "with administrator privileges") {
		t.Errorf("script: %s", script)
	}
	if want := []string{e.path("daemon"), e.path("helper"), e.path("socket"), e.path("agent")}; !slices.Equal(r.Removed, want) || len(r.Failed) != 0 {
		t.Fatalf("removed %v failed %v, want %v", r.Removed, r.Failed, want)
	}
	if !e.exists("config") || !e.exists("app") {
		t.Fatal("the config or the app bundle was removed")
	}
}

func TestTunnelHubRemoveCancelledAtAdminPromptChangesNothing(t *testing.T) {
	e := newTHEnv(t, thAll...)
	e.cancel = true
	r, err := e.s.Remove(context.Background())
	if err != nil || r.Status != TunnelHubCancelled || len(r.Removed) != 0 {
		t.Fatalf("Remove = %+v, %v", r, err)
	}
	if len(e.calls) != 1 {
		t.Fatalf("ran %v after a cancelled admin prompt", e.calls)
	}
	for _, k := range thAll {
		if !e.exists(k) {
			t.Fatalf("%s removed after a cancelled admin prompt", k)
		}
	}
}

func TestTunnelHubRemoveAgentNeedsNoAdmin(t *testing.T) {
	e := newTHEnv(t, "agent", "config")
	r, err := e.s.Remove(context.Background())
	if err != nil || r.Status != TunnelHubRemoved {
		t.Fatalf("Remove = %+v, %v", r, err)
	}
	want := [][]string{{"/bin/launchctl", "bootout", "gui/501", e.path("agent")}}
	if !slices.EqualFunc(e.calls, want, slices.Equal) {
		t.Fatalf("calls %v, want %v", e.calls, want)
	}
	if e.exists("agent") || !e.exists("config") {
		t.Fatal("the agent plist stayed or the config went")
	}
	if !slices.Equal(r.Removed, []string{e.path("agent")}) {
		t.Fatalf("removed %v", r.Removed)
	}
}

func TestTunnelHubRemoveReportsWhatStayed(t *testing.T) {
	e := newTHEnv(t, "daemon", "socket")
	e.s.run = func(name string, args ...string) (int, string, error) {
		e.calls = append(e.calls, append([]string{name}, args...))
		os.Remove(e.path("daemon")) // the socket stays
		return 1, "rm: Operation not permitted", nil
	}
	r, err := e.s.Remove(context.Background())
	if err != nil || r.Status != TunnelHubRemoved {
		t.Fatalf("Remove = %+v, %v", r, err)
	}
	if !slices.Equal(r.Removed, []string{e.path("daemon")}) || !slices.Equal(r.Failed, []string{e.path("socket")}) {
		t.Fatalf("removed %v failed %v", r.Removed, r.Failed)
	}
}

func TestTunnelHubRemoveRefusesUnsafePaths(t *testing.T) {
	e := newTHEnv(t)
	e.s.root = filepath.Join(t.TempDir(), "it's")
	e.put(t, "helper")
	if _, err := e.s.Remove(context.Background()); err == nil {
		t.Fatal("Remove ran with a quote in a path")
	}
	if len(e.calls) != 0 || len(e.dialogs) != 0 || !e.exists("helper") {
		t.Fatalf("calls %v dialogs %d", e.calls, len(e.dialogs))
	}
}
