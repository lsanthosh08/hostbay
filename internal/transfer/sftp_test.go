package transfer_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"hostbay/internal/sftp"
	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/transfer"
	"hostbay/internal/transfer/sourcetest"
)

// newServer starts an in-process SSH server, skipping where it cannot run.
func newServer(t *testing.T) *sftptest.Server {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	return sftptest.New(t, "pw")
}

func sftpSource(t *testing.T, s *sftptest.Server) transfer.Source {
	t.Helper()
	var p sftp.Pool
	c, err := p.Get(s.Dial(t))
	if err != nil {
		t.Fatal(err)
	}
	return transfer.NewSFTP(c)
}

func TestSFTPPassesTheSourceSuite(t *testing.T) {
	sourcetest.Run(t, sftpSource(t, newServer(t)), tempDir(t))
}

func TestSFTPOverwriteKeepsModeAndSymlink(t *testing.T) {
	s := newServer(t)
	local, remote := tempDir(t), tempDir(t)
	writeFile(t, local+"/tool", "new tool", 0o644)
	writeFile(t, local+"/link.sh", "new script", 0o644)
	writeFile(t, remote+"/tool", "old tool", 0o755)
	writeFile(t, remote+"/real.sh", "old script", 0o755)
	if err := os.Symlink("real.sh", remote+"/link.sh"); err != nil {
		t.Fatal(err)
	}

	e, ch := newEngine()
	id := e.Start(transfer.Local{}, []string{local + "/tool", local + "/link.sh"}, sftpSource(t, s), remote)
	evs := wait(t, ch, id, answerAll(t, e, transfer.Overwrite, true))
	if last := evs[len(evs)-1]; last.State != "done" || last.Files != 2 {
		t.Fatalf("final %+v", last.Event)
	}
	if fi, _ := os.Lstat(remote + "/link.sh"); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link replaced by a file")
	}
	for p, want := range map[string]string{"tool": "new tool", "real.sh": "new script"} {
		fi, _ := os.Stat(filepath.Join(remote, p))
		if got := readFile(t, remote+"/"+p); got != want || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s = %q mode %v", p, got, fi.Mode())
		}
	}
	if got := dirNames(t, remote); len(got) != 3 {
		t.Fatalf("remote holds %v", got)
	}
}
