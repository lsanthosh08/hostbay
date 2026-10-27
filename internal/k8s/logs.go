package k8s

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Logs follows a pod container's log from its last tail lines. The stream
// is a read-only terminal transport, so the shared registry's emit loop
// carries it to the page (KTD9) and closing the cluster ends it.
func (c *Client) Logs(ctx context.Context, ns, pod, container string, tail int) (*LogStream, error) {
	k, err := target("pods", ns, pod)
	if err != nil {
		return nil, err
	}
	if err := checkLabel("container", container); err != nil {
		return nil, err
	}
	if tail <= 0 || tail > 100000 {
		tail = 1000
	}
	q := url.Values{"follow": {"true"}, "container": {container}, "tailLines": {strconv.Itoa(tail)}}
	// Not ctx: the stream outlives the call that opened it, until Close.
	resp, err := c.request(context.WithoutCancel(ctx), http.MethodGet, k.path(ns, pod)+"/log", q, nil, "")
	if err != nil {
		return nil, err
	}
	return &LogStream{body: resp.Body}, nil
}

// LogStream is a followed log.
type LogStream struct{ body io.ReadCloser }

func (l *LogStream) Read(b []byte) (int, error) { return l.body.Read(b) }

func (l *LogStream) Write([]byte) (int, error) { return 0, errors.New("a log takes no input") }

func (l *LogStream) Resize(int, int) error { return nil }

func (l *LogStream) Close() error { return l.body.Close() }

func (l *LogStream) EndReason() string { return "log ended" }
