package sync

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// objectStore is an HTTP object store with ETags, which is what WebDAV and S3
// both are as far as sync goes.
type objectStore interface {
	// request sends method for name with body and extra headers.
	request(ctx context.Context, method, name string, body []byte, h http.Header) (*http.Response, error)
}

var errNotFound = errors.New("not found")

// casObjects is a shared Backend over an object store: the snapshot is
// replaced only with If-Match on the ETag Fetch saw, or created only with
// If-None-Match: * when there was none, so a concurrent upload gets 412.
type casObjects struct {
	o    objectStore
	what string // for messages: "WebDAV", "S3"
}

func (c casObjects) get(ctx context.Context, name string) ([]byte, string, error) {
	res, err := c.o.request(ctx, http.MethodGet, name, nil, nil)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, "", errNotFound
	}
	if res.StatusCode != http.StatusOK {
		return nil, "", c.status("read", res)
	}
	etag := res.Header.Get("ETag")
	if etag == "" {
		return nil, "", fmt.Errorf("%s target gives no ETag, so uploads cannot be checked; choose another target", c.what)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	return data, etag, err
}

func (c casObjects) Fetch(ctx context.Context) ([]File, string, error) {
	data, etag, err := c.get(ctx, SharedName)
	if errors.Is(err, errNotFound) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	return []File{{Name: SharedName, Data: data}}, etag, nil
}

func (c casObjects) Store(ctx context.Context, name string, data []byte, version string) error {
	h := http.Header{}
	if version == "" {
		h.Set("If-None-Match", "*")
	} else {
		h.Set("If-Match", version)
	}
	res, err := c.o.request(ctx, http.MethodPut, name, data, h)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusPreconditionFailed:
		return ErrConflict
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return nil
	}
	return c.status("upload", res)
}

func (c casObjects) Version(ctx context.Context) (string, error) {
	res, err := c.o.request(ctx, http.MethodHead, SharedName, nil, nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNotFound:
		return "", nil
	case http.StatusOK:
		return res.Header.Get("ETag"), nil
	}
	return "", c.status("check", res)
}

func (c casObjects) Prune(context.Context) error { return nil }
func (c casObjects) PerDevice() bool             { return false }
func (c casObjects) Close() error                { return nil }

// probe creates a scratch object twice with If-None-Match: *. A target
// that lets the second through ignores conditional uploads (KTD13).
func (c casObjects) probe(ctx context.Context) error {
	name := ".hostbay-probe-" + rand.Text()
	defer func() {
		if res, err := c.o.request(ctx, http.MethodDelete, name, nil, nil); err == nil {
			res.Body.Close()
		}
	}()
	if err := c.Store(ctx, name, []byte("probe"), ""); err != nil {
		return fmt.Errorf("cannot write to the %s target: %w", c.what, err)
	}
	if err := c.Store(ctx, name, []byte("probe"), ""); !errors.Is(err, ErrConflict) {
		return fmt.Errorf("this %s target ignores conditional uploads, so two computers could overwrite each other; choose another target", c.what)
	}
	return nil
}

func (c casObjects) status(op string, res *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	err := fmt.Errorf("%s %s: %s %s", c.what, op, res.Status, body)
	if _, s3 := c.o.(*s3Object); s3 && res.StatusCode == http.StatusForbidden {
		// S3 answers 403 for much besides refused keys; its error code tells.
		switch s3Code(body) {
		case "InvalidAccessKeyId", "SignatureDoesNotMatch", "AccessDenied":
			return fmt.Errorf("%w: %w", ErrAuth, err)
		case "RequestTimeTooSkewed":
			return fmt.Errorf("%w: %w", ErrClock, err)
		}
		return err
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		err = fmt.Errorf("%w: %w", ErrAuth, err)
	}
	return err
}

// s3Code reads <Code> from an S3 XML error body, which may be cut short.
func s3Code(body []byte) string {
	_, rest, _ := strings.Cut(string(body), "<Code>")
	code, _, _ := strings.Cut(rest, "</Code>")
	return code
}
