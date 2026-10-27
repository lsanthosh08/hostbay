package agentexec

import (
	"bytes"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// payload reads back what a POSIX envelope hands to eval.
func payload(t *testing.T, env []byte) string {
	t.Helper()
	line := strings.TrimSuffix(string(env), "\r")
	if !strings.HasPrefix(line, " eval ") {
		t.Fatalf("envelope %q does not start with a space and eval", env)
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(line), "")
	if err != nil {
		t.Fatal(err)
	}
	call := f.Stmts[0].Cmd.(*syntax.CallExpr)
	if len(call.Args) != 2 {
		t.Fatalf("eval has %d words", len(call.Args))
	}
	fields, err := expand.Fields(&expand.Config{}, call.Args[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 {
		t.Fatalf("eval's argument is %d fields", len(fields))
	}
	return fields[0]
}

func TestEnvelopeUnwrapsToTheApprovedBytes(t *testing.T) {
	for _, cmd := range []string{"ls", `printf '%s\n' "it's"`, `echo '''`, "a'b'c", `$(x) "$y" \z`} {
		env := Envelope(ShellPOSIX, cmd, "n0nce")
		if got := payload(t, env); got != cmd {
			t.Errorf("payload of %q = %q", cmd, got)
		}
	}
}

// The echoed command line never holds the assembled marker, so only the
// shell's own printf can end the job.
func TestEnvelopeMarkerIsNotInTheEchoedLine(t *testing.T) {
	env := Envelope(ShellPOSIX, "ls", "n0nce")
	if bytes.Contains(env, marker("n0nce")) || !bytes.Contains(env, []byte("n0nce")) {
		t.Fatalf("envelope %q", env)
	}
	ps := Envelope(ShellPowerShell, "Get-Date", "n0nce")
	if !bytes.HasPrefix(ps, []byte(" & { Get-Date }")) || bytes.Contains(ps, marker("n0nce")) {
		t.Fatalf("PowerShell envelope %q", ps)
	}
	if raw := Envelope(ShellNone, "show version", "n0nce"); string(raw) != "show version\r" {
		t.Fatalf("confirm-only shells get the bytes as they are, got %q", raw)
	}
}

// The scanner finds the marker split across chunks and reads the status.
func TestScannerFindsASplitMarker(t *testing.T) {
	m := string(marker("n0nce")) + "17\a"
	sc := newScanner("n0nce")
	if _, ok := sc.feed([]byte("output " + m[:5])); ok {
		t.Fatal("found early")
	}
	code, ok := sc.feed([]byte(m[5:] + "prompt$ "))
	if !ok || code != 17 {
		t.Fatalf("code %d, %v", code, ok)
	}
	if _, ok := newScanner("n0nce").feed([]byte(string(marker("other")) + "0\a")); ok {
		t.Fatal("another job's marker ended this one")
	}
}
