//go:build integration

package database

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"hostbay/internal/apperr"
	"hostbay/internal/dockertest"
	"hostbay/internal/tlstrust"
)

// An Elasticsearch 8 node reachable only from the far end of the chain,
// with the self-signed certificate it generates, raises the prompt, then
// pins that certificate as a leaf, lists indices, runs a request, and
// replaces a document by its _id.
func TestElasticsearchThroughATwoHopChain(t *testing.T) {
	dockertest.Require(t)
	network := dockertest.Network(t)
	c := chainClient(t, network)
	name := dockertest.Name("hostbay-es")
	const pw = "it-Es-Pass-7!" // a throwaway container's password
	// The generated certificate names the node's host name, which the
	// chain reaches it by.
	dockertest.Start(t, name, "--network", network, "--hostname", name, "-e", "discovery.type=single-node", "-e", "ELASTIC_PASSWORD="+pw,
		"-e", "ES_JAVA_OPTS=-Xms512m -Xmx512m", "docker.elastic.co/elasticsearch/elasticsearch:8.15.3")
	dockertest.WaitExec(t, name, 4*time.Minute, "curl", "-sfk", "-u", "elastic:"+pw, "https://localhost:9200/_cluster/health?wait_for_status=yellow&timeout=5s")
	dial := &chainDial{c: c, want: net.JoinHostPort(name, "9200")}
	certs := tlstrust.New(t.TempDir())
	ctx := context.Background()
	open := func() (Engine, error) {
		return OpenElasticsearch(ctx, ESConfig{URL: "https://" + name + ":9200", User: "elastic", Password: pw, Dial: dial.dial,
			Trust: func(try func(*tls.Config) error) (*tls.Config, error) {
				return certs.Capture("elasticsearch", "h1", name, 9200, try)
			}})
	}
	_, err := open()
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != apperr.HostKeyUnknown || ae.Details["kind"] != "elasticsearchCert" {
		t.Fatalf("err = %v, want the elasticsearchCert prompt", err)
	}
	if err := certs.Answer(ae.Details["requestId"], true); err != nil {
		t.Fatal(err)
	}
	e, err := open()
	if err != nil {
		t.Fatalf("the pinned generated certificate: %v", err)
	}
	defer e.Close()

	allRows(t, e, "PUT /logs/_doc/1?refresh=true\n{\"msg\": \"hello\", \"n\": 12345678901234567}")
	indices, _, err := e.Children(ctx, nil, 0)
	if err != nil || !hasNode(indices, "logs") {
		t.Fatalf("indices = %+v, %v", indices, err)
	}
	ed := e.(DocumentEditor)
	if rows := allRows(t, e, ed.SelectTable([]string{"logs"})); len(rows) != 1 || rows[0][1].Text != "1" || !strings.Contains(rows[0][2].Text, "hello") {
		t.Fatalf("hits = %+v", rows)
	}
	if err := ed.EditDocument(ctx, []string{"logs"}, "1", `{"msg": "bye", "n": 12345678901234567}`); err != nil {
		t.Fatal(err)
	}
	doc, err := ed.Document(ctx, []string{"logs"}, "1")
	if err != nil || !strings.Contains(doc, `"bye"`) || !strings.Contains(doc, "12345678901234567") {
		t.Fatalf("after the edit: %s, %v", doc, err)
	}
	if err := ed.EditDocument(ctx, []string{"logs"}, "2", `{"msg": "new"}`); err == nil {
		t.Fatal("an edit of a missing document saved")
	}
	if rows := allRows(t, e, "GET /_cluster/health"); !strings.Contains(rows[0][1].Text, "cluster_name") {
		t.Fatalf("health = %+v", rows)
	}
	dial.check(t)
}
