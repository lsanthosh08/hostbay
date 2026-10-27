package sync

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// NewWebDAV syncs through a WebDAV folder (Nextcloud, ownCloud, Apache
// mod_dav and the like). The snapshot is replaced only with If-Match on its
// ETag, so a concurrent upload from another computer gets 412 (KTD13).
func NewWebDAV(t Target, password string, client *http.Client) (Backend, error) {
	u, err := url.Parse(strings.TrimRight(t.URL, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("enter the WebDAV folder's http(s) URL")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return casObjects{o: &davObject{base: u.String(), user: t.User, password: password, client: client}, what: "WebDAV"}, nil
}

type davObject struct {
	base, user, password string
	client               *http.Client
}

func (d *davObject) do(ctx context.Context, method, target string, body []byte, h http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	if d.user != "" || d.password != "" {
		req.SetBasicAuth(d.user, d.password)
	}
	return d.client.Do(req)
}

func (d *davObject) request(ctx context.Context, method, name string, body []byte, h http.Header) (*http.Response, error) {
	target := d.base + "/" + url.PathEscape(name)
	res, err := d.do(ctx, method, target, body, h)
	if err != nil || method != http.MethodPut || res.StatusCode != http.StatusConflict {
		return res, err
	}
	// 409 on PUT means the folder does not exist yet: create it and retry.
	res.Body.Close()
	mk, err := d.do(ctx, "MKCOL", d.base+"/", nil, nil)
	if err != nil {
		return nil, err
	}
	mk.Body.Close()
	return d.do(ctx, method, target, body, h)
}
