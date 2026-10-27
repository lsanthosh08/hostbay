package agentexec

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The agent reaches a terminal or an exec channel only through this
// package's gate (KTD32): no other import gives internal/ai a way to write
// to a terminal or run a command.
var forbidden = []string{"hostbay/internal/terminal", "hostbay/internal/app", "hostbay/internal/sshx", "hostbay/internal/admin"}

func TestAgentReachesTerminalsOnlyThroughTheGate(t *testing.T) {
	for _, dir := range []string{"../ai", "../ai/aitest", "../mcp"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		if dir == "../ai" && len(files) == 0 {
			t.Fatal("internal/ai has no files to check")
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			if bad := forbiddenImport(t, f); bad != "" {
				t.Errorf("%s imports %s", f, bad)
			}
		}
	}
}

func forbiddenImport(t *testing.T, path string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		for _, bad := range forbidden {
			if p == bad || strings.HasPrefix(p, bad+"/") {
				return p
			}
		}
	}
	return ""
}
