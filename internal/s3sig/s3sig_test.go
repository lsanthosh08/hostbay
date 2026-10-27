package s3sig

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rhnvrm/simples3"
)

// The SigV4 signer must agree with simples3's for the same request.
func TestSignMatchesSimples3(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	s3 := simples3.New("eu-west-1", "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	s3.SetEndpoint(srv.URL)
	rc, err := s3.FileDownload(simples3.DownloadInput{Bucket: "bkt", ObjectKey: "dir/hostbay.sync"})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()

	when, err := time.Parse("20060102T150405Z", got.Header.Get("X-Amz-Date"))
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+got.URL.Path, nil)
	for _, h := range []string{"Date", "X-Amz-Date", "X-Amz-Content-Sha256"} {
		req.Header.Set(h, got.Header.Get(h))
	}
	Sign(req, got.Header.Get("X-Amz-Content-Sha256"), "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "eu-west-1", when)
	if req.Header.Get("Authorization") != got.Header.Get("Authorization") {
		t.Fatalf("signature differs:\n ours   %s\n simples3 %s", req.Header.Get("Authorization"), got.Header.Get("Authorization"))
	}
}

func TestStreamSendsTheBodyUnsignedWithItsLengthAndACanonicalQuery(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	b := &Bucket{Endpoint: u, Name: "bkt", PathStyle: true, Access: "AK", Secret: "SK"}
	q := url.Values{"prefix": {"a b/c/"}, "delimiter": {"/"}, "list-type": {"2"}}
	res, err := b.Stream(context.Background(), http.MethodPut, "dir/x y.txt", q, strings.NewReader("hello"), 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got.Header.Get("X-Amz-Content-Sha256") != UnsignedPayload || got.ContentLength != 5 || body != "hello" {
		t.Fatalf("sha %q length %d body %q", got.Header.Get("X-Amz-Content-Sha256"), got.ContentLength, body)
	}
	if got.URL.EscapedPath() != "/bkt/dir/x%20y.txt" || got.URL.RawQuery != "delimiter=%2F&list-type=2&prefix=a%20b%2Fc%2F" {
		t.Fatalf("path %q query %q", got.URL.EscapedPath(), got.URL.RawQuery)
	}
}

func TestVirtualHostedURLPutsTheBucketInTheHost(t *testing.T) {
	u, _ := url.Parse("https://s3.eu-west-1.amazonaws.com")
	b := &Bucket{Endpoint: u, Name: "bkt"}
	if got := b.URL("a/b c").String(); got != "https://bkt.s3.eu-west-1.amazonaws.com/a/b%20c" {
		t.Fatal(got)
	}
}
