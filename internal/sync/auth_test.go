package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cloudsoda/go-smb2"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"hostbay/internal/sftp/sftptest"
	"hostbay/internal/sshx"
)

// A refused sign-in pauses sync, so the background tick does not retry it
// every minute and lock the account on servers that count failures.
func TestRefusedSignInPausesSync(t *testing.T) {
	refuse := func(code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	sftpSrv := sftptest.New(t, "pw")
	dialer := &sshx.Dialer{KnownHostsPath: knownHostsFile(t, sftpSrv.KnownHostsLine())}
	for name, target := range map[string]Target{
		"webdav": {Kind: "webdav", URL: refuse(http.StatusUnauthorized) + "/dav", User: "u"},
		"s3":     s3Refusing(t, "InvalidAccessKeyId"),
		"sftp":   sftpTarget(t, sftpSrv.Addr, filepath.Join(t.TempDir(), "sync")),
	} {
		t.Run(name, func(t *testing.T) {
			// Object stores sign each request, so the refusal comes on the first read.
			b, err := Connect(context.Background(), target, "wrong", Deps{Dialer: dialer})
			if err == nil {
				_, _, err = b.Fetch(context.Background())
				b.Close()
			}
			if err == nil || PauseCode(err) != CodeAuth {
				t.Fatalf("err = %v, pause %q; want %q", err, PauseCode(err), CodeAuth)
			}
		})
	}
	for name, err := range map[string]error{
		"smb logon":     &smb2.ResponseError{Code: 0xC000006D}, // STATUS_LOGON_FAILURE
		"smb locked":    fmt.Errorf("dial: %w", &smb2.ResponseError{Code: 0xC0000234}),
		"git refused":   fmt.Errorf("git fetch: %w", transport.ErrAuthorizationFailed),
		"git anonymous": fmt.Errorf("git fetch: %w", transport.ErrAuthenticationRequired),
	} {
		if PauseCode(err) != CodeAuth {
			t.Errorf("%s: pause %q, want %q", name, PauseCode(err), CodeAuth)
		}
	}
	for name, err := range map[string]error{
		"smb missing file": &smb2.ResponseError{Code: 0xC0000034}, // STATUS_OBJECT_NAME_NOT_FOUND
		"network":          errors.New("dial tcp: connection refused"),
	} {
		if got := PauseCode(err); got != "" {
			t.Errorf("%s: pause %q, want none", name, got)
		}
	}
}

// s3Refusing is an S3 target that answers every request 403 with code.
func s3Refusing(t *testing.T, code string) Target {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n<Error><Code>%s</Code><Message>refused</Message></Error>", code)
	}))
	t.Cleanup(srv.Close)
	return Target{Kind: "s3", URL: srv.URL, Bucket: "b", User: "AKID", Region: "eu-west-1"}
}

// S3 answers 403 for refused keys and for much else; only the codes that
// mean the keys were refused pause for sign-in, and a skewed clock pauses
// with its own code. The background check is a HEAD, whose 403 has no body.
func TestS3RefusalsPauseByErrorCode(t *testing.T) {
	for code, want := range map[string]string{
		"InvalidAccessKeyId":    CodeAuth,
		"SignatureDoesNotMatch": CodeAuth,
		"AccessDenied":          CodeAuth,
		"RequestTimeTooSkewed":  CodeClock,
		"SlowDown":              "",
	} {
		t.Run(code, func(t *testing.T) {
			b, err := Connect(context.Background(), s3Refusing(t, code), "wrong", Deps{})
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			_, _, ferr := b.Fetch(context.Background())
			_, verr := b.Version(context.Background())
			for op, err := range map[string]error{"Fetch": ferr, "Version": verr} {
				if err == nil || PauseCode(err) != want {
					t.Errorf("%s: err = %v, pause %q; want %q", op, err, PauseCode(err), want)
				}
			}
		})
	}
}
