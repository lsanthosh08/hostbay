package claudecode

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// sources parses the package's own files, leaving out tests.
func sources(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	out := map[string]*ast.File{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, b, 0) // no comments: prose may name what the code avoids
		if err != nil {
			t.Fatal(err)
		}
		out[name] = f
	}
	if len(out) == 0 {
		t.Fatal("no source files found")
	}
	return fset, out
}

// Hostbay never reads Claude's own credential files or the system keychain
// (R2, KTD6): the program does its own sign-in, and this package only runs
// the program. A string that names one of those places is the way such a
// read would start.
func TestNoSourceNamesAClaudeCredentialLocationOrTheKeychain(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)\.claude|\.credentials|credentials\.json|keychain|find-generic-password|security-?framework|secret-?tool|wincred|credman`)
	fset, files := sources(t)
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err == nil && forbidden.MatchString(s) {
				t.Errorf("%s: string %q names a credential location", fset.Position(lit.Pos()), s)
			}
			return true
		})
		_ = name
	}
}

// The program is started with fixed argument lists and never with a
// sign-in or sign-out word (R2): signing in is Anthropic's flow in the
// person's own terminal.
func TestNoSourceStartsASignIn(t *testing.T) {
	_, files := sources(t)
	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && (s == "login" || s == "logout" || s == "setup-token") {
				t.Errorf("%s: %q is passed as an argument", name, s)
			}
			return true
		})
	}
}

// The package may not reach a terminal or a secret: a model call has no
// tools, so what it could touch is what it is handed on stdin (KTD1, R3).
func TestThePackageImportsNeitherTerminalsNorSecrets(t *testing.T) {
	_, files := sources(t)
	for name, f := range files {
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasSuffix(p, "/internal/terminal") || strings.HasSuffix(p, "/internal/secrets") ||
				strings.HasSuffix(p, "/internal/agentexec") || strings.HasSuffix(p, "/internal/store") {
				t.Errorf("%s imports %s", name, p)
			}
		}
	}
}
