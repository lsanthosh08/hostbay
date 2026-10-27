package transfer_test

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/transfer"
	"hostbay/internal/transfer/sourcetest"
)

// scpServer is a test server whose clients use SCP; it needs the scp
// command on this machine, since the server runs the real one.
func scpServer(t *testing.T) *sftptest.Server {
	t.Helper()
	s := newServer(t)
	if _, err := exec.LookPath("scp"); err != nil {
		t.Skip("scp is not installed; the SCP tests run the real scp as the server side")
	}
	s.RefuseSFTP()
	return s
}

func TestSCPPassesTheSourceSuite(t *testing.T) {
	s := scpServer(t)
	sourcetest.Run(t, transfer.NewSCP(s.Dial(t)), tempDir(t))
}

func TestSCPCarriesEmptyFilesAndOddNames(t *testing.T) {
	s := scpServer(t)
	src := transfer.NewSCP(s.Dial(t))
	local, remote, back := tempDir(t), tempDir(t), tempDir(t)
	names := []string{"empty", `it's a "quoted" name.txt`, "  lead and trail  ", "-dash"}
	if err := os.Mkdir(local+"/up", 0o755); err != nil {
		t.Fatal(err)
	}
	for i, n := range names {
		writeFile(t, local+"/up/"+n, strings.Repeat("z", i*3), 0o644)
	}

	e, ch := newEngine()
	evs := wait(t, ch, e.Start(transfer.Local{}, []string{local + "/up"}, src, remote), nil)
	if last := evs[len(evs)-1]; last.State != "done" || last.Files != len(names) {
		t.Fatalf("upload %+v", last.Event)
	}
	evs = wait(t, ch, e.Start(src, []string{remote + "/up"}, transfer.Local{}, back), nil)
	if last := evs[len(evs)-1]; last.State != "done" || last.Files != len(names) {
		t.Fatalf("download %+v", last.Event)
	}
	for i, n := range names {
		if got := readFile(t, back+"/up/"+n); got != strings.Repeat("z", i*3) {
			t.Errorf("%q = %q", n, got)
		}
	}
	if !slices.ContainsFunc(s.Commands(), func(c string) bool { return strings.HasPrefix(c, "scp -t ") }) {
		t.Fatalf("no scp upload ran: %q", s.Commands())
	}
}

// evilSCP answers scp -f as a hostile server would, naming the file
// ../evil (CVE-2019-6111).
const evilSCP = `#!/bin/sh
head -c 1 >/dev/null
printf 'C0644 4 ../evil\n'
head -c 1 >/dev/null
printf 'evil\0'
head -c 1 >/dev/null
`

func TestSCPRefusesATraversalNameFromTheServer(t *testing.T) {
	s := scpServer(t)
	s.Command(t, "scp", evilSCP)
	src := transfer.NewSCP(s.Dial(t))
	remote, parent := tempDir(t), tempDir(t)
	writeFile(t, remote+"/x.txt", "fine", 0o644)
	dest := parent + "/dest"
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	if r, err := src.Open(context.Background(), remote+"/x.txt"); err == nil {
		r.Close()
		t.Fatal("Open accepted the name ../evil")
	}
	e, ch := newEngine()
	evs := wait(t, ch, e.Start(src, []string{remote + "/x.txt"}, transfer.Local{}, dest), nil)
	last := evs[len(evs)-1]
	if last.State != "failed" || len(last.Failed) != 1 {
		t.Fatalf("final %+v", last.Event)
	}
	if got := dirNames(t, parent); len(got) != 1 {
		t.Fatalf("written beside the destination: %v", got)
	}
	if got := dirNames(t, dest); len(got) != 0 {
		t.Fatalf("destination holds %v", got)
	}
}

// blankSCP answers scp -f with a blank line, as a shell rc that prints
// for non-interactive shells would.
const blankSCP = `#!/bin/sh
head -c 1 >/dev/null
printf '\n'
`

func TestSCPRefusesABlankLineFromTheServer(t *testing.T) {
	s := scpServer(t)
	s.Command(t, "scp", blankSCP)
	src := transfer.NewSCP(s.Dial(t))
	remote := tempDir(t)
	writeFile(t, remote+"/x.txt", "fine", 0o644)
	if r, err := src.Open(context.Background(), remote+"/x.txt"); err == nil {
		r.Close()
		t.Fatal("Open accepted a blank line")
	}
}
