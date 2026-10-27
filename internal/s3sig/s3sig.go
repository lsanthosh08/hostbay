// Package s3sig signs requests to S3-compatible object stores (AWS, MinIO,
// R2, B2 ...) with AWS Signature Version 4. Sync and the S3 file source
// share it.
//
// github.com/rhnvrm/simples3 v0.11.1 cannot send conditional headers, which
// is presumably why uniterm builds against a fork; rather than replace the
// module, Hostbay signs plain net/http requests itself.
package s3sig

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// UnsignedPayload stands in for the body's hash when the body is streamed,
// so it is read only once, as it is sent.
const UnsignedPayload = "UNSIGNED-PAYLOAD"

// Bucket addresses one bucket.
type Bucket struct {
	Endpoint *url.URL // the service's http(s) URL
	Name     string
	// PathStyle puts the bucket in the path (endpoint/bucket/key), as MinIO
	// and most self-hosted stores expect; otherwise it is a subdomain
	// (bucket.endpoint/key), as AWS prefers.
	PathStyle              bool
	Access, Secret, Region string
	Client                 *http.Client
}

// Do sends a signed request for key ("" for the bucket itself) with body,
// whose hash is signed too.
func (b *Bucket) Do(ctx context.Context, method, key string, query url.Values, body []byte, h http.Header) (*http.Response, error) {
	sum := sha256.Sum256(body)
	return b.send(ctx, method, key, query, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]), h)
}

// Stream sends a signed request whose body of size bytes is read as it is
// sent, under UnsignedPayload.
func (b *Bucket) Stream(ctx context.Context, method, key string, query url.Values, body io.Reader, size int64, h http.Header) (*http.Response, error) {
	return b.send(ctx, method, key, query, body, size, UnsignedPayload, h)
}

func (b *Bucket) send(ctx context.Context, method, key string, query url.Values, body io.Reader, size int64, payload string, h http.Header) (*http.Response, error) {
	u := b.URL(key)
	u.RawQuery = canonicalQuery(query)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	for k, v := range h {
		req.Header[k] = v
	}
	req.Header.Set("X-Amz-Content-Sha256", payload)
	region := b.Region
	if region == "" {
		region = "us-east-1"
	}
	Sign(req, payload, b.Access, b.Secret, region, time.Now().UTC())
	c := b.Client
	if c == nil {
		c = http.DefaultClient
	}
	return c.Do(req)
}

// URL is where key lives, path-style or virtual-hosted.
func (b *Bucket) URL(key string) *url.URL {
	u := *b.Endpoint
	base := strings.TrimRight(u.Path, "/")
	rawBase := strings.TrimRight(u.EscapedPath(), "/")
	if b.PathStyle {
		u.Path = base + "/" + b.Name + "/" + key
		u.RawPath = rawBase + "/" + Escape(b.Name) + "/" + Escape(key)
	} else {
		u.Host = b.Name + "." + u.Host
		u.Path = base + "/" + key
		u.RawPath = rawBase + "/" + Escape(key)
	}
	return &u
}

// Sign signs req with AWS Signature Version 4 for S3, covering host and
// every header already set (so conditional headers cannot be stripped).
// payload is the hex SHA-256 of the body, or UnsignedPayload. req's query
// must already be in canonical form, as Bucket builds it.
func Sign(req *http.Request, payload, access, secret, region string, now time.Time) {
	amzDate, day := now.Format("20060102T150405Z"), now.Format("20060102")
	if req.Header.Get("X-Amz-Date") == "" {
		req.Header.Set("X-Amz-Date", amzDate)
	}
	if req.Header.Get("X-Amz-Content-Sha256") == "" {
		req.Header.Set("X-Amz-Content-Sha256", payload)
	}

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		v = slices.Clone(v)
		slices.Sort(v)
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	slices.Sort(names)
	var canon strings.Builder
	for _, n := range names {
		canon.WriteString(n + ":" + headers[n] + "\n")
	}
	signed := strings.Join(names, ";")
	creq := strings.Join([]string{req.Method, req.URL.EscapedPath(), req.URL.RawQuery, canon.String(), signed, payload}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	h := sha256.Sum256([]byte(creq))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	k := hmacSHA256([]byte("AWS4"+secret), day)
	for _, part := range []string{region, "s3", "aws4_request"} {
		k = hmacSHA256(k, part)
	}
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+access+"/"+scope+", SignedHeaders="+signed+
		", Signature="+hex.EncodeToString(hmacSHA256(k, toSign)))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// Escape percent-encodes a key as SigV4 requires: everything but
// unreserved characters, keeping "/".
func Escape(s string) string { return escape(s, "-._~/") }

// canonicalQuery encodes q sorted by name, each name and value encoded as
// SigV4 requires ("/" included), so the query sent is the query signed.
func canonicalQuery(q url.Values) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(q)) {
		vs := slices.Sorted(slices.Values(q[k]))
		for _, v := range vs {
			parts = append(parts, escape(k, "-._~")+"="+escape(v, "-._~"))
		}
	}
	return strings.Join(parts, "&")
}

func escape(s, keep string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte(keep, c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}
