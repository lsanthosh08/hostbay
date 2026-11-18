package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/portowner"
)

// An AWS Client VPN SSO sign-in ends with the identity provider's form post
// of a SAMLResponse to 127.0.0.1:35001, a port AWS fixes. Hostbay listens
// there only while a sign-in waits, and treats the response as a password:
// it never goes into a log, an error or a reply.

var (
	// awsSSOAddr is where the browser posts the sign-in; tests change it.
	awsSSOAddr = "127.0.0.1:35001"
	// awsSSOWait is how long a sign-in waits for the browser.
	awsSSOWait = 5 * time.Minute
)

// awsSSOMaxBody caps a posted form; a SAMLResponse is a few tens of KB.
const awsSSOMaxBody = 256 << 10

// The reason detail of an AWS sign-in's VPNFailed error. A busy lock or port
// is not a failed sign-in: retrying cannot help until the user acts.
const (
	awsSSOReasonBusy    = "signin-busy"
	awsSSOReasonPort    = "port-busy"
	awsSSOReasonTimeout = "signin-timeout"
)

// awsSSOLock lets one AWS sign-in run at a time: two would need the same
// port, and a retry must not open a second browser window.
var awsSSOLock sync.Mutex

// takeAWSSSOLock takes the sign-in lock for a whole connect attempt, from
// the first OpenVPN run until the tunnel is up or the attempt ends. It fails
// at once while another sign-in holds it. release may be called more than
// once.
func takeAWSSSOLock() (release func(), err error) {
	if !awsSSOLock.TryLock() {
		return nil, apperr.New(apperr.VPNFailed, "Another AWS VPN sign-in is in progress. Finish it in the browser, then connect again.").
			With("reason", awsSSOReasonBusy)
	}
	return sync.OnceFunc(awsSSOLock.Unlock), nil
}

// awsSSOListener takes the one SAMLResponse a sign-in posts.
type awsSSOListener struct {
	srv       *http.Server
	host      string // the Host a request must name: the bound address
	taken     atomic.Bool
	got       chan string // the response, once
	closeOnce sync.Once
}

// listenAWSSSO listens for the sign-in's form post. A port another app
// holds fails with that app's name where the system can tell.
func listenAWSSSO() (*awsSSOListener, error) {
	ln, err := net.Listen("tcp", awsSSOAddr)
	if err != nil {
		_, port, _ := net.SplitHostPort(awsSSOAddr)
		if !portowner.InUse(err) {
			return nil, apperr.New(apperr.VPNFailed, fmt.Sprintf("Hostbay cannot listen on port %s for the AWS VPN sign-in: %v", port, err))
		}
		msg := fmt.Sprintf("Port %s is in use by another app, and AWS VPN sign-in needs it. Quit that app, then connect again.", port)
		if p, perr := strconv.Atoi(port); perr == nil {
			if name, nerr := portowner.Name(p); nerr == nil {
				msg = fmt.Sprintf("Port %s is in use by %s, and AWS VPN sign-in needs it. Quit %s, then connect again.", port, name, name)
			}
		}
		return nil, apperr.New(apperr.VPNFailed, msg).With("reason", awsSSOReasonPort)
	}
	l := &awsSSOListener{host: ln.Addr().String(), got: make(chan string, 1)}
	l.srv = &http.Server{Handler: l, ReadHeaderTimeout: 5 * time.Second}
	go l.srv.Serve(ln)
	return l, nil
}

// ServeHTTP accepts one form post naming this listener as its Host. An
// Origin is allowed: the identity provider's page posts from its own site.
// A request it refuses gets a fixed reply and the sign-in keeps waiting.
func (l *awsSSOListener) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Host != l.host:
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	case r.Method != http.MethodPost:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, awsSSOMaxBody)
	// The parse error may quote the body, so it is not passed on.
	if err := r.ParseForm(); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	resp := r.PostForm.Get("SAMLResponse")
	if resp == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !l.taken.CompareAndSwap(false, true) {
		http.Error(w, "this sign-in has already finished", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "close")
	io.WriteString(w, awsSSODonePage)
	l.got <- resp
}

const awsSSODonePage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Hostbay</title></head>
<body style="font-family: system-ui, sans-serif; margin: 3em">
<h1>Signed in</h1>
<p>Return to Hostbay. You can close this tab.</p>
</body></html>
`

// wait returns the posted SAMLResponse. It ends at awsSSOWait or when ctx
// does, and closes the listener in every case; the caller still releases
// the sign-in lock.
func (l *awsSSOListener) wait(ctx context.Context) (string, error) {
	defer l.close()
	t := time.NewTimer(awsSSOWait)
	defer t.Stop()
	select {
	case resp := <-l.got:
		return resp, nil
	case <-t.C:
		return "", apperr.New(apperr.VPNFailed, "The AWS VPN sign-in was not completed in the browser in time.").
			With("reason", awsSSOReasonTimeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// close stops listening, letting a reply in progress finish first. It may
// be called more than once.
func (l *awsSSOListener) close() {
	l.closeOnce.Do(func() {
		l.taken.Store(true) // a post racing the close must not be told it signed in
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if l.srv.Shutdown(ctx) != nil {
			l.srv.Close()
		}
	})
}
