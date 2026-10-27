package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"hostbay/internal/sshx"
	"hostbay/internal/store"
	"hostbay/internal/tlstrust"
	"hostbay/internal/transfer"
)

// LocalSource is the source ID of this computer's disk. Any other source ID
// is a session ID that ConnService.Connect returned, or one TransferConnect
// returned for a file host.
const LocalSource = "local"

// TransferSource describes a source opened for a file pane.
type TransferSource struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`           // local, sftp, scp, ftp, ftps, smb, webdav or s3
	Home string `json:"home,omitempty"` // the folder a pane starts in: the local user's home, or "/"
}

// TransferService moves files between any two sources, the local disk, a
// session's SFTP or SCP, or an FTP, SMB, WebDAV or S3 host, with progress,
// cancel and a choice on name conflicts (R38–R44). Progress arrives as
// TransferEvent.
type TransferService struct {
	files *FilesService
	creds *HostCredentials
	certs *tlstrust.Store
	eng   *transfer.Engine
	emit  func(name string, data any)

	mu    sync.Mutex
	hosts map[string]fileHost // source ID -> an open file host
}

type fileHost struct {
	src  transfer.Source
	kind string
}

// NewTransferService shares files' SFTP clients, so a session keeps one
// client and one memory of a refused SFTP subsystem. File hosts sign in
// with creds and pin certificates in certs (KTD18).
func NewTransferService(files *FilesService, creds *HostCredentials, certs *tlstrust.Store) *TransferService {
	s := &TransferService{files: files, creds: creds, certs: certs, emit: emitEvent, hosts: map[string]fileHost{}}
	s.eng = transfer.New(func(ev transfer.Event) { s.emit(TransferEvent, ev) })
	return s
}

func (s *TransferService) source(sourceID string) (transfer.Source, string, error) {
	if sourceID == LocalSource {
		return transfer.Local{}, "local", nil
	}
	s.mu.Lock()
	fh, ok := s.hosts[sourceID]
	s.mu.Unlock()
	if ok {
		return fh.src, fh.kind, nil
	}
	return s.files.source(sourceID)
}

// TransferConnect signs in to a saved FTP, SMB, WebDAV or S3 host and
// returns it as a source, whose ID the other calls take. An FTPS or https
// server whose certificate is not known yet fails with HostKeyUnknown
// (details.kind "ftpCert", "webdavCert" or "s3Cert"): answer it with
// TransferTrustCert and connect again. A changed one fails with
// HostKeyChanged naming both fingerprints. Either way no password was sent.
// FTPS never falls back to plain FTP.
func (s *TransferService) TransferConnect(ctx context.Context, hostID string) (TransferSource, error) {
	c, err := s.creds.Lookup(hostID)
	if err != nil {
		return TransferSource{}, err
	}
	src, kind, err := s.dial(ctx, c)
	if err != nil {
		return TransferSource{}, err
	}
	id := "files-" + rand.Text()
	s.mu.Lock()
	s.hosts[id] = fileHost{src: src, kind: kind}
	s.mu.Unlock()
	return TransferSource{ID: id, Kind: kind, Home: "/"}, nil
}

// dial connects to a file host as its saved settings say, and only so:
// through its proxy when it names one, never around it (KTD22). A file
// host has no way through a jump host, so one that names a jump host fails.
func (s *TransferService) dial(ctx context.Context, c Credentials) (transfer.Source, string, error) {
	h := c.Host
	if h.Jumped() {
		return nil, "", errors.New("a file host cannot be reached through a jump host; remove its jump host or connect it through a proxy")
	}
	d, err := s.creds.st.Data()
	if err != nil {
		return nil, "", err
	}
	p, err := s.creds.proxyFor(d, &h)
	if err != nil {
		return nil, "", err
	}
	var via transfer.DialFunc
	if p != nil {
		via = func(ctx context.Context, _, addr string) (net.Conn, error) { return sshx.DialTCP(ctx, p, addr) }
	}
	port := func(def int) int {
		if h.Port != 0 {
			return h.Port
		}
		return def
	}
	switch h.KindName() {
	case "ftp":
		mode := h.FTPTLS()
		p := port(21)
		if mode == "implicit" {
			p = port(990)
		}
		cfg := transfer.FTPConfig{Addr: net.JoinHostPort(h.Hostname, strconv.Itoa(p)), User: h.User, Password: c.Password, Implicit: mode == "implicit", Dial: via}
		var src *transfer.FTP
		try := func(tc *tls.Config) (err error) {
			cfg.TLS = tc
			src, err = transfer.DialFTP(ctx, cfg)
			return err
		}
		switch mode {
		case "none":
			err = try(nil)
		case "explicit", "implicit":
			_, err = s.certs.Connect("ftp", h.ID, h.Hostname, p, try)
		default:
			err = fmt.Errorf("unknown FTP TLS mode %q", mode)
		}
		if err != nil {
			return nil, "", err
		}
		if mode == "none" {
			return src, "ftp", nil
		}
		return src, "ftps", nil
	case "smb":
		sess, err := transfer.DialSMB(ctx, via, net.JoinHostPort(h.Hostname, strconv.Itoa(port(445))), h.User, c.Password, store.Deref(h.Domain))
		if err != nil {
			return nil, "", err
		}
		return transfer.NewSMB(sess), "smb", nil
	case "webdav":
		var src *transfer.WebDAV
		err := s.overHTTP(h, func(tc *tls.Config) (err error) {
			src, err = transfer.DialWebDAV(ctx, transfer.WebDAVConfig{URL: store.Deref(h.URL), User: h.User, Password: c.Password, TLS: tc, Dial: via})
			return err
		})
		if err != nil {
			return nil, "", err
		}
		return src, "webdav", nil
	case "s3":
		var src *transfer.S3
		err := s.overHTTP(h, func(tc *tls.Config) (err error) {
			src, err = transfer.DialS3(ctx, transfer.S3Config{Endpoint: store.Deref(h.URL), Bucket: store.Deref(h.Bucket), Region: store.Deref(h.Region),
				PathStyle: h.PathStyle != nil && *h.PathStyle, Access: h.User, Secret: c.Password, TLS: tc, Dial: via})
			return err
		})
		if err != nil {
			return nil, "", err
		}
		return src, "s3", nil
	}
	return nil, "", fmt.Errorf("a %s host has no file transfer of its own; connect it as a session", h.KindName())
}

// overHTTP runs try for a WebDAV or S3 host: through the pin store for an
// https:// URL, without TLS only for an http:// one the user chose.
func (s *TransferService) overHTTP(h store.Host, try func(*tls.Config) error) error {
	raw := store.Deref(h.URL)
	if strings.HasPrefix(raw, "http://") {
		return try(nil)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return errors.New("enter the server's http(s) URL")
	}
	p := 443
	if u.Port() != "" {
		p, _ = strconv.Atoi(u.Port())
	}
	_, err = s.certs.Connect(h.KindName(), h.ID, u.Hostname(), p, try)
	return err
}

// TransferTrustCert answers the HostKeyUnknown prompt (details.kind
// "ftpCert", "webdavCert" or "s3Cert") carrying requestID; accepting pins
// the certificate.
func (s *TransferService) TransferTrustCert(requestID string, accept bool) error {
	return s.certs.Answer(requestID, accept)
}

// TransferDisconnect closes a file host TransferConnect opened.
func (s *TransferService) TransferDisconnect(sourceID string) error {
	s.mu.Lock()
	fh, ok := s.hosts[sourceID]
	delete(s.hosts, sourceID)
	s.mu.Unlock()
	if !ok {
		return errors.New("this file host is not connected")
	}
	if c, ok := fh.src.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// TransferOpen reports which protocol a source uses, so a pane can say when
// a host fell back to SCP, and the home folder a local pane starts in.
func (s *TransferService) TransferOpen(ctx context.Context, sourceID string) (TransferSource, error) {
	_, kind, err := s.source(sourceID)
	if err != nil {
		return TransferSource{}, err
	}
	out := TransferSource{ID: sourceID, Kind: kind}
	if kind == "local" {
		home, err := os.UserHomeDir()
		if err != nil {
			return TransferSource{}, err
		}
		out.Home = filepath.ToSlash(home)
	}
	return out, nil
}

// TransferList lists a folder of a source. Paths are slash-separated on
// every source, the local disk included.
func (s *TransferService) TransferList(ctx context.Context, sourceID, dir string) ([]DirEntry, error) {
	src, _, err := s.source(sourceID)
	if err != nil {
		return nil, err
	}
	entries, err := src.List(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, len(entries))
	for i, e := range entries {
		out[i] = DirEntry(e)
	}
	return out, nil
}

// TransferStart copies paths of one source, files or whole folders, into
// the folder toDir of another, and returns the task ID its events carry.
func (s *TransferService) TransferStart(ctx context.Context, fromID string, paths []string, toID, toDir string) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("nothing to transfer")
	}
	from, _, err := s.source(fromID)
	if err != nil {
		return "", err
	}
	to, _, err := s.source(toID)
	if err != nil {
		return "", err
	}
	return s.eng.Start(from, paths, to, toDir), nil
}

// TransferCancel stops a task; the file in progress is discarded.
func (s *TransferService) TransferCancel(taskID string) error { return s.eng.Cancel(taskID) }

// TransferAnswer answers a conflict question with overwrite, skip or
// rename; with all, the task's later conflicts get the same answer.
func (s *TransferService) TransferAnswer(requestID, choice string, all bool) error {
	return s.eng.Answer(requestID, transfer.Choice(choice), all)
}

// TransferMkdir makes the folder name inside dir.
func (s *TransferService) TransferMkdir(ctx context.Context, sourceID, dir, name string) error {
	src, _, err := s.source(sourceID)
	if err != nil {
		return err
	}
	return src.Mkdir(ctx, dir, name)
}

// TransferRename renames or moves a file or folder within one source.
func (s *TransferService) TransferRename(ctx context.Context, sourceID, from, to string) error {
	src, _, err := s.source(sourceID)
	if err != nil {
		return err
	}
	return src.Rename(ctx, from, to)
}

// TransferRemove deletes a file or an empty folder.
func (s *TransferService) TransferRemove(ctx context.Context, sourceID, p string, isDir bool) error {
	src, _, err := s.source(sourceID)
	if err != nil {
		return err
	}
	return src.Remove(ctx, p, isDir)
}
