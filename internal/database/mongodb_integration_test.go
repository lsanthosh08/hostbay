//go:build integration

package database

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"hostbay/internal/dockertest"
)

// A MongoDB server reachable only from the far end of the chain signs in,
// lists databases and collections, pages a find, and replaces a document
// by its _id, storing a field named "$set" as a field.
func TestMongoThroughATwoHopChain(t *testing.T) {
	dockertest.Require(t)
	network := dockertest.Network(t)
	c := chainClient(t, network)
	name := dockertest.Name("hostbay-mongo")
	const pw = "it-Mongo-Pass-6!" // a throwaway container's password
	dockertest.Start(t, name, "--network", network, "-e", "MONGO_INITDB_ROOT_USERNAME=root", "-e", "MONGO_INITDB_ROOT_PASSWORD="+pw, "mongo:7")
	dockertest.WaitExec(t, name, 2*time.Minute, "mongosh", "--quiet", "-u", "root", "-p", pw, "--eval", "db.runCommand({ping: 1})")
	dial := &chainDial{c: c, want: net.JoinHostPort(name, "27017")}
	ctx := context.Background()
	e, err := OpenMongo(ctx, MongoConfig{Host: name, Port: 27017, User: "root", Password: pw, TLS: "none", Dial: dial.dial})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	var docs strings.Builder
	for i := range 700 {
		if i > 0 {
			docs.WriteString(",")
		}
		docs.WriteString(`{"_id": ` + strconv.Itoa(i) + `, "n": {"$numberLong": "` + strconv.Itoa(i) + `"}}`)
	}
	allRows(t, e, `{"$db": "app", "insert": "items", "documents": [`+docs.String()+`]}`)
	dbs, _, err := e.Children(ctx, nil, 0)
	if err != nil || !hasNode(dbs, "app") {
		t.Fatalf("databases = %+v, %v", dbs, err)
	}
	colls, _, err := e.Children(ctx, []string{"app"}, 0)
	if err != nil || !hasNode(colls, "items") {
		t.Fatalf("collections = %+v, %v", colls, err)
	}
	ed := e.(DocumentEditor)
	if rows := allRows(t, e, ed.SelectTable([]string{"app", "items"})); len(rows) != 700 || rows[0][0].Text != `{"$numberInt":"0"}` {
		t.Fatalf("find paged %d documents, first %+v", len(rows), rows[:1])
	}

	id := `{"$numberInt":"3"}`
	doc, err := ed.Document(ctx, []string{"app", "items"}, id)
	if err != nil || !strings.Contains(doc, `"n":{"$numberLong":"3"}`) {
		t.Fatalf("document = %s, %v", doc, err)
	}
	if err := ed.EditDocument(ctx, []string{"app", "items"}, id, `{"_id": {"$numberInt": "3"}, "$set": {"n": 1}, "n": {"$numberLong": "30"}}`); err != nil {
		t.Fatal(err)
	}
	doc, err = ed.Document(ctx, []string{"app", "items"}, id)
	if err != nil || !strings.Contains(doc, `"$set":{"n":{"$numberInt":"1"}}`) || !strings.Contains(doc, `"n":{"$numberLong":"30"}`) {
		t.Fatalf("after the edit: %s, %v", doc, err)
	}
	if err := ed.EditDocument(ctx, []string{"app", "items"}, `{"$numberInt":"9999"}`, `{"n": 1}`); err == nil {
		t.Fatal("an edit of a missing document saved")
	}
	if n := allRows(t, e, `{"$db": "app", "count": "items", "query": {"_id": 9999}}`); !strings.Contains(n[0][0].Text, `"n":{"$numberInt":"0"}`) {
		t.Fatalf("the edit created a document: %+v", n)
	}
	dial.check(t)
}

// A replica set's primary lists another member; the direct connection
// never dials it.
func TestMongoReplicaSetMemberIsNeverDialled(t *testing.T) {
	dockertest.Require(t)
	network := dockertest.Network(t)
	c := chainClient(t, network)
	name := dockertest.Name("hostbay-mongo-rs")
	dockertest.Start(t, name, "--network", network, "--hostname", name, "mongo:7", "--replSet", "rs0", "--bind_ip_all")
	dockertest.WaitExec(t, name, 2*time.Minute, "mongosh", "--quiet", "--eval", "db.runCommand({ping: 1})")
	dockertest.Docker(t, "exec", name, "mongosh", "--quiet", "--eval",
		`rs.initiate({_id: "rs0", members: [{_id: 0, host: "`+name+`:27017"}]})`)
	dockertest.WaitExec(t, name, time.Minute, "mongosh", "--quiet", "--eval", "if (!db.hello().isWritablePrimary) quit(1)")
	dockertest.Docker(t, "exec", name, "mongosh", "--quiet", "--eval",
		`rs.add({host: "ghost.invalid:27017", priority: 0, votes: 0})`)
	dial := &chainDial{c: c, want: net.JoinHostPort(name, "27017")}
	e, err := OpenMongo(context.Background(), MongoConfig{Host: name, Port: 27017, TLS: "none", Dial: dial.dial})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if rows := allRows(t, e, `{"replSetGetConfig": 1}`); !strings.Contains(rows[0][0].Text, "ghost.invalid") {
		t.Fatalf("the set does not list the other member: %+v", rows)
	}
	time.Sleep(2 * time.Second) // a replica-set client would be dialling the member by now
	dial.check(t)
}
