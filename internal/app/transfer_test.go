package app

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/webdav"

	"hostbay/internal/apperr"
	"hostbay/internal/secrets"
	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx"
	"hostbay/internal/store"
	"hostbay/internal/tlstrust"
	"hostbay/internal/tlstrust/tlstest"
	"hostbay/internal/transfer"
)

// scpOnlyServer is a test server that refuses SFTP, so clients fall back to
// the real scp it runs (R38).
func scpOnlyServer(t *testing.T) *sftptest.Server {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	if _, err := exec.LookPath("scp"); err != nil {
		t.Skip("scp is not installed; the SCP tests run the real scp as the server side")
	}
	s := sftptest.New(t, "login-pw")
	s.RefuseSFTP()
	return s
}

func newTransfers(t *testing.T, s *sftptest.Server) (*TransferService, *sshx.Lease, chan transfer.Event) {
	t.Helper()
	f, l := newFilesSession(t, s)
	ts := NewTransferService(f, nil, nil)
	ch := make(chan transfer.Event, 1000)
	ts.emit = func(name string, data any) {
		if name == TransferEvent {
			ch <- data.(transfer.Event)
		}
	}
	return ts, l, ch
}

// slashTemp is a fresh folder in slash form with symlinks resolved.
func slashTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(d)
}

func put(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAE17FolderUploadOverSCPSkipsExistingFilesForAll(t *testing.T) {
	s := scpOnlyServer(t)
	ts, l, ch := newTransfers(t, s)
	ctx := context.Background()
	local, remote := slashTemp(t), slashTemp(t)
	put(t, local+"/site/a.txt", "new a")
	put(t, local+"/site/b.txt", "new b")
	put(t, local+"/site/sub/c.txt", "new c")
	put(t, remote+"/site/a.txt", "old a")
	put(t, remote+"/site/sub/c.txt", "old c")
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	for _, p := range []string{remote + "/site/a.txt", remote + "/site/sub/c.txt"} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	src, err := ts.TransferOpen(ctx, l.ID())
	if err != nil || src.Kind != "scp" {
		t.Fatalf("open = %+v, %v", src, err)
	}
	id, err := ts.TransferStart(ctx, LocalSource, []string{local + "/site"}, l.ID(), remote)
	if err != nil {
		t.Fatal(err)
	}
	asked := 0
	var final transfer.Event
	for final.State == "" || final.State == "running" || final.State == "conflict" {
		select {
		case ev := <-ch:
			if ev.TaskID != id {
				continue
			}
			final = ev
			if ev.State == "conflict" {
				asked++
				if err := ts.TransferAnswer(ev.Conflict.RequestID, "skip", true); err != nil {
					t.Fatal(err)
				}
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("transfer did not end; last %+v", final)
		}
	}

	slices.Sort(final.Skipped)
	if want := []string{remote + "/site/a.txt", remote + "/site/sub/c.txt"}; final.State != "done" || !slices.Equal(final.Skipped, want) {
		t.Fatalf("final %+v, want skipped %v", final, want)
	}
	if asked != 1 {
		t.Fatalf("asked %d times after skip for all", asked)
	}
	for p, want := range map[string]string{"site/a.txt": "old a", "site/sub/c.txt": "old c", "site/b.txt": "new b"} {
		b, err := os.ReadFile(remote + "/" + p)
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", p, b, err)
		}
	}
	for _, p := range []string{"site/a.txt", "site/sub/c.txt"} {
		if fi, _ := os.Stat(remote + "/" + p); !fi.ModTime().Equal(old) {
			t.Errorf("%s was rewritten (mtime %v)", p, fi.ModTime())
		}
	}
	if !slices.ContainsFunc(s.Commands(), func(c string) bool { return strings.HasPrefix(c, "scp -t ") }) {
		t.Fatalf("not sent over scp: %q", s.Commands())
	}
	if n := s.SFTPOpens(); n != 1 {
		t.Fatalf("SFTP asked for %d times on one connection", n)
	}
}

func TestTransferOpenNamesEachSourceKind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test server runs commands with /bin/sh")
	}
	ts, l, _ := newTransfers(t, sftptest.New(t, "login-pw"))
	ctx := context.Background()
	if src, err := ts.TransferOpen(ctx, l.ID()); err != nil || src.Kind != "sftp" || src.ID != l.ID() {
		t.Fatalf("remote = %+v, %v", src, err)
	}
	home, _ := os.UserHomeDir()
	if src, err := ts.TransferOpen(ctx, LocalSource); err != nil || src.Kind != "local" || src.Home != filepath.ToSlash(home) {
		t.Fatalf("local = %+v, %v", src, err)
	}
	if _, err := ts.TransferOpen(ctx, "nope"); err == nil {
		t.Fatal("unknown session opened")
	}
	if _, err := ts.TransferStart(ctx, "nope", []string{"/x"}, LocalSource, "/"); err == nil {
		t.Fatal("transfer started from an unknown session")
	}
}

func TestFilesFallBackToSCPWhenSFTPIsRefused(t *testing.T) {
	s := scpOnlyServer(t)
	f, l := newFilesSession(t, s)
	id := l.ID()
	remote, local := slashTemp(t), slashTemp(t)
	put(t, local+"/up.txt", "uploaded")

	if err := f.SftpMkdir(id, remote+"/d"); err != nil {
		t.Fatal(err)
	}
	if err := f.SftpUpload(id, local+"/up.txt", remote+"/d/f.txt"); err != nil {
		t.Fatal(err)
	}
	if err := f.SftpWrite(id, remote+"/d/f.txt", "edited", false, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := f.SftpRead(id, remote+"/d/f.txt"); err != nil || got != "edited" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if err := f.SftpChmod(id, remote+"/d/f.txt", 0o750, false); err != nil {
		t.Fatal(err)
	}
	if err := f.SftpRename(id, remote+"/d/f.txt", remote+"/d/g.txt"); err != nil {
		t.Fatal(err)
	}
	es, err := f.SftpList(id, remote+"/d")
	if err != nil || len(es) != 1 || es[0].Name != "g.txt" || es[0].Mode&0o777 != 0o750 {
		t.Fatalf("list = %+v, %v", es, err)
	}
	if err := f.SftpDownload(id, remote+"/d/g.txt", local+"/down.txt"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(local + "/down.txt"); string(b) != "edited" {
		t.Fatalf("downloaded %q", b)
	}
	if err := f.SftpRemove(id, remote+"/d/g.txt", false, false); err != nil {
		t.Fatal(err)
	}
	if err := f.SftpRemove(id, remote+"/d", true, false); err != nil {
		t.Fatal(err)
	}
	if err := f.SftpWrite(id, remote+"/x", "x", true, nil); err == nil {
		t.Fatal("sudo save claimed to work without SFTP")
	}
	if n := s.SFTPOpens(); n != 1 {
		t.Fatalf("SFTP asked for %d times on one connection", n)
	}
}

// davHost serves a temp folder over WebDAV, over TLS with cert when it is
// set, to user u with password pw, and counts requests carrying credentials.
func davHost(t *testing.T, cert *tls.Certificate) (*httptest.Server, string, *atomic.Int32) {
	t.Helper()
	dir := t.TempDir()
	h := &webdav.Handler{FileSystem: webdav.Dir(dir), LockSystem: webdav.NewMemLS()}
	var authed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, ok := r.BasicAuth()
		if ok {
			authed.Add(1)
		}
		if user != "u" || pw != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	if cert != nil {
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, dir, &authed
}

func fileHostTransfers(t *testing.T) (*HostService, *TransferService, chan transfer.Event) {
	t.Helper()
	hosts, creds, dir := newHostService(t, fakeVerifier(secrets.Passed))
	ts := NewTransferService(NewFilesService(sshx.NewManager(&sshx.Dialer{})), creds, tlstrust.New(dir))
	ch := make(chan transfer.Event, 1000)
	ts.emit = func(name string, data any) {
		if name == TransferEvent {
			ch <- data.(transfer.Event)
		}
	}
	return hosts, ts, ch
}

func TestFileHostBrowsesAndTransfersLikeASession(t *testing.T) {
	srv, remote, _ := davHost(t, nil)
	hosts, ts, ch := fileHostTransfers(t)
	kind, url, pw := "webdav", srv.URL+"/", "pw"
	h, err := hosts.SaveHost(Host{Alias: "dav", User: "u", Kind: &kind, URL: &url, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	src, err := ts.TransferConnect(ctx, h.ID)
	if err != nil || src.Kind != "webdav" || src.Home != "/" {
		t.Fatalf("connect = %+v, %v", src, err)
	}
	if open, err := ts.TransferOpen(ctx, src.ID); err != nil || open.Kind != "webdav" {
		t.Fatalf("open = %+v, %v", open, err)
	}
	if err := ts.TransferMkdir(ctx, src.ID, "/", "in"); err != nil {
		t.Fatal(err)
	}
	local := slashTemp(t)
	put(t, local+"/a.txt", "hello")
	id, err := ts.TransferStart(ctx, LocalSource, []string{local + "/a.txt"}, src.ID, "/in")
	if err != nil {
		t.Fatal(err)
	}
	for ev := range ch {
		if ev.TaskID == id && ev.State != "running" {
			if ev.State != "done" {
				t.Fatalf("final %+v", ev)
			}
			break
		}
	}
	if b, err := os.ReadFile(filepath.Join(remote, "in", "a.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("uploaded %q, %v", b, err)
	}
	es, err := ts.TransferList(ctx, src.ID, "/in")
	if err != nil || len(es) != 1 || es[0].Name != "a.txt" || es[0].Path != "/in/a.txt" {
		t.Fatalf("list = %+v, %v", es, err)
	}
	if err := ts.TransferDisconnect(src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.TransferList(ctx, src.ID, "/"); err == nil {
		t.Fatal("a disconnected file host still lists")
	}
}

func TestFileHostAsksForItsCertificateBeforeSendingThePassword(t *testing.T) {
	cert := tlstest.Cert(t)
	srv, _, authed := davHost(t, &cert)
	hosts, ts, _ := fileHostTransfers(t)
	kind, url, pw := "webdav", srv.URL, "pw"
	h, err := hosts.SaveHost(Host{Alias: "dav", User: "u", Kind: &kind, URL: &url, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ts.TransferConnect(context.Background(), h.ID)
	ae, ok := err.(*apperr.Error)
	if !ok || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "webdavCert" || authed.Load() != 0 {
		t.Fatalf("err = %v, %d requests carried the password", err, authed.Load())
	}
	if err := ts.TransferTrustCert(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	if src, err := ts.TransferConnect(context.Background(), h.ID); err != nil || src.Kind != "webdav" {
		t.Fatalf("after pinning: %+v, %v", src, err)
	}
}

// R29, KTD18: a password saved for FTPS must not go out over plain FTP.
func TestEditingFTPSToPlainFTPAsksForThePasswordAgain(t *testing.T) {
	hosts, creds, _ := newHostService(t, fakeVerifier(secrets.Passed))
	kind, explicit, none, pw := "ftp", "explicit", "none", "ftp-secret"
	h, err := hosts.SaveHost(Host{Alias: "files", Hostname: "ftp.lan", Port: 21, User: "u", Kind: &kind, TLS: &explicit, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := creds.Lookup(h.ID); err != nil || c.Password != pw {
		t.Fatalf("FTPS lookup = %q, %v", c.Password, err)
	}
	h.TLS, h.Password = &none, nil
	if _, err := hosts.SaveHost(h); err != nil {
		t.Fatal(err)
	}
	if _, err := creds.Lookup(h.ID); apperr.CodeOf(err) != apperr.SecretNeeded {
		t.Fatalf("plain FTP lookup = %v, want SecretNeeded", err)
	}
}

func TestSaveHostRefusesFileHostsThatCannotConnect(t *testing.T) {
	hosts, _, _ := newHostService(t, fakeVerifier(secrets.Passed))
	str := func(s string) *string { return &s }
	for name, h := range map[string]Host{
		"unknown FTP TLS mode": {Kind: str("ftp"), TLS: str("maybe")},
		"WebDAV without URL":   {Kind: str("webdav")},
		"WebDAV ftp:// URL":    {Kind: str("webdav"), URL: str("ftp://dav.lan")},
		"S3 without bucket":    {Kind: str("s3"), URL: str("https://s3.lan"), User: "AK"},
		"S3 without key":       {Kind: str("s3"), URL: str("https://s3.lan"), Bucket: str("b")},
	} {
		if _, err := hosts.SaveHost(h); err == nil {
			t.Errorf("%s was saved", name)
		}
	}
}

// A file host has no way through a jump host, so one that names a jump
// host fails rather than dial the server directly (KTD22), and saving one
// is refused.
func TestFileHostWithAJumpHostNeverDialsDirectly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var dialled atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dialled.Add(1)
			c.Close()
		}
	}()
	hosts, ts, _ := fileHostTransfers(t)
	jump, err := hosts.SaveHost(Host{Alias: "bastion", Hostname: "bastion.lan", Port: 22, User: "u"})
	if err != nil {
		t.Fatal(err)
	}
	kind, none, pw := "ftp", "none", "pw"
	port := ln.Addr().(*net.TCPAddr).Port
	h, err := hosts.SaveHost(Host{Alias: "files", Hostname: "127.0.0.1", Port: port, User: "u", Kind: &kind, TLS: &none, Password: &pw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hosts.SaveHost(Host{Alias: "jumped", Hostname: "127.0.0.1", Port: port, User: "u", Kind: &kind, TLS: &none, JumpHostID: &jump.ID}); err == nil {
		t.Fatal("a file host with a jump host was saved")
	}
	// One left that way by an import or an older build.
	if err := hosts.st.Edit(func(d *store.Data) error {
		return d.EditHost(h.ID, func(sh *store.Host) { sh.JumpHostID = &jump.ID })
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.TransferConnect(context.Background(), h.ID); err == nil {
		t.Fatal("a jumped file host connected")
	}
	time.Sleep(50 * time.Millisecond)
	if n := dialled.Load(); n != 0 {
		t.Fatalf("the server was dialled directly %d times", n)
	}
}
