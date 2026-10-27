package sync

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"hostbay/internal/s3sig"
)

// NewS3 syncs through an S3-compatible bucket (AWS, MinIO, R2, B2 ...),
// path-style at t.URL/t.Bucket/t.Path. Uploads are conditional PUTs
// (If-Match / If-None-Match), and sync refuses to turn on where they are
// ignored (KTD13). Requests are signed by internal/s3sig.
func NewS3(t Target, secretKey string, client *http.Client) (Backend, error) {
	u, err := url.Parse(strings.TrimRight(t.URL, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("enter the S3 endpoint's http(s) URL")
	}
	if t.Bucket == "" || t.User == "" || secretKey == "" {
		return nil, errors.New("S3 sync needs a bucket, an access key and a secret key")
	}
	b := &s3sig.Bucket{Endpoint: u, Name: t.Bucket, PathStyle: true, Access: t.User, Secret: secretKey, Region: t.Region, Client: client}
	return casObjects{o: &s3Object{b: b, prefix: strings.Trim(t.Path, "/")}, what: "S3"}, nil
}

type s3Object struct {
	b      *s3sig.Bucket
	prefix string
}

func (o *s3Object) request(ctx context.Context, method, name string, body []byte, h http.Header) (*http.Response, error) {
	key := name
	if o.prefix != "" {
		key = o.prefix + "/" + name
	}
	res, err := o.b.Do(ctx, method, key, nil, body, h)
	// A HEAD response has no body to carry S3's error code, so a refused
	// check is repeated as a GET, whose 403 does.
	if err == nil && method == http.MethodHead && res.StatusCode == http.StatusForbidden {
		res.Body.Close()
		return o.request(ctx, http.MethodGet, name, nil, h)
	}
	return res, err
}
