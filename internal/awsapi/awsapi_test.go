package awsapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// isolate points every AWS file and the home folder at a temp dir, so no
// test reads this computer's own AWS setup.
func isolate(t *testing.T, config, credentials string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if body != "" {
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	t.Setenv("AWS_CONFIG_FILE", write("config", config))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", write("credentials", credentials))
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, k := range []string{"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_REGION", "AWS_DEFAULT_REGION"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	return dir
}

const sampleConfig = `
# comment
[profile zeta]
region = eu-west-1
source_profile = sso-base
role_arn = arn:aws:iam::123456789012:role/sample

[default]
region = us-west-2

[profile sso-base]
sso_session = corp
sso_account_id = 123456789012
sso_role_name = Sample

[sso-session corp]
sso_start_url = https://sample.example.test/start
sso_region = us-east-1

[profile loop-a]
source_profile = loop-b
[profile loop-b]
source_profile = loop-a

[profile mfa-role]
source_profile = keys-only
role_arn = arn:aws:iam::123456789012:role/admin
mfa_serial = arn:aws:iam::123456789012:mfa/sample
s3 =
  max_concurrent_requests = 4
`

const sampleCredentials = `
[keys-only]
aws_access_key_id = AKIDSAMPLE
aws_secret_access_key = sample-secret-key
mfa_serial = arn:aws:iam::123456789012:mfa/sample
`

func byName(ps []Profile) map[string]Profile {
	m := map[string]Profile{}
	for _, p := range ps {
		m[p.Name] = p
	}
	return m
}

func TestProfiles(t *testing.T) {
	isolate(t, sampleConfig, sampleCredentials)
	ps, err := Profiles()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range ps {
		names = append(names, p.Name)
	}
	want := []string{"default", "keys-only", "loop-a", "loop-b", "mfa-role", "sso-base", "zeta"}
	if !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v (default first, then sorted, no sso-session)", names, want)
	}
	m := byName(ps)
	if m["default"].Region != "us-west-2" || m["zeta"].Region != "eu-west-1" {
		t.Errorf("regions: %+v", m)
	}
	if r := m["sso-base"]; r.SSORoot != "sso-base" || r.ssoKey != "corp" {
		t.Errorf("sso-base root = %q key %q", r.SSORoot, r.ssoKey)
	}
	if z := m["zeta"]; z.SSORoot != "sso-base" || z.ssoKey != "corp" {
		t.Errorf("zeta should inherit its source_profile's SSO root: %+v", z)
	}
	if m["loop-a"].SSORoot != "" || m["keys-only"].SSORoot != "" {
		t.Errorf("no SSO root expected: %+v %+v", m["loop-a"], m["keys-only"])
	}
	// keys-only has an mfa_serial but assumes no role, so the SDK never
	// asks for a code, and mfa-role's chain stops at its keys.
	if !m["mfa-role"].MFA || m["zeta"].MFA || m["keys-only"].MFA {
		t.Errorf("mfa flags: mfa-role %v zeta %v", m["mfa-role"].MFA, m["zeta"].MFA)
	}
}

func TestProfilesLegacySSOAndMissingFiles(t *testing.T) {
	isolate(t, "[profile old]\nsso_start_url = https://old.example.test/start\n", "")
	ps, err := Profiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].SSORoot != "old" || ps[0].ssoKey != "https://old.example.test/start" {
		t.Fatalf("legacy SSO profile: %+v", ps)
	}

	isolate(t, "", "[only]\naws_access_key_id = AKIDSAMPLE\n")
	ps, err = Profiles()
	if err != nil || len(ps) != 1 || ps[0].Name != "only" {
		t.Fatalf("missing config file: %+v %v", ps, err)
	}
}

func TestSignInNeeded(t *testing.T) {
	dir := isolate(t, sampleConfig, "")
	ps, _ := Profiles()
	p := byName(ps)["zeta"]
	if need, _ := SignInNeeded(p); !need {
		t.Fatal("no cached token should need a sign-in")
	}
	cache := filepath.Join(dir, ".aws", "sso", "cache")
	os.MkdirAll(cache, 0o700)
	path, err := cachedTokenPath("corp")
	if err != nil || filepath.Dir(path) != cache {
		t.Fatalf("token path %q %v", path, err)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	os.WriteFile(path, []byte(`{"accessToken":"sample","expiresAt":"`+past+`"}`), 0o600)
	if need, at := SignInNeeded(p); !need || at != past {
		t.Fatalf("expired token: need %v at %q", need, at)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	os.WriteFile(path, []byte(`{"accessToken":"sample","expiresAt":"`+future+`"}`), 0o600)
	if need, _ := SignInNeeded(p); need {
		t.Fatal("a valid token needs no sign-in")
	}
	if need, _ := SignInNeeded(byName(ps)["keys-only"]); need {
		t.Fatal("a profile without SSO never needs an SSO sign-in")
	}
}

// fakeAWS serves canned bodies by Action and records each request's form.
func fakeAWS(t *testing.T, bodies map[string][]string) *[]*http.Request {
	t.Helper()
	var got []*http.Request
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = append(got, r)
		a := r.PostForm.Get("Action")
		list := bodies[a]
		if len(list) == 0 {
			http.Error(w, "unexpected "+a, http.StatusBadRequest)
			return
		}
		i := min(calls[a], len(list)-1)
		calls[a]++
		body := list[i]
		if strings.HasPrefix(body, "!") { // an error reply
			w.WriteHeader(http.StatusForbidden)
			body = body[1:]
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	old := endpoint
	endpoint = func(service, region string) string { return srv.URL + "/" + service + "/" + region }
	t.Cleanup(func() { endpoint = old })
	return &got
}

func testClient(t *testing.T) *Client {
	isolate(t, "[profile keys]\nregion = us-east-1\n", "[keys]\naws_access_key_id = AKIDSAMPLE\naws_secret_access_key = sample-secret-key\n")
	c, err := New(context.Background(), "keys", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const instancesPage1 = `<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
<reservationSet><item><instancesSet><item>
  <instanceId>i-0123456789abcdef0</instanceId><instanceType>t3.micro</instanceType>
  <instanceState><code>16</code><name>running</name></instanceState>
  <privateIpAddress>192.0.2.10</privateIpAddress><ipAddress>198.51.100.7</ipAddress>
  <keyName>sample-key</keyName>
  <groupSet><item><groupId>sg-1</groupId></item></groupSet>
  <tagSet><item><key>Owner</key><value>ops</value></item><item><key>Name</key><value>web-1</value></item></tagSet>
</item></instancesSet></item></reservationSet>
<nextToken>page-2</nextToken></DescribeInstancesResponse>`

const instancesPage2 = `<DescribeInstancesResponse><reservationSet><item><instancesSet><item>
  <instanceId>i-0fedcba9876543210</instanceId><instanceType>m5.large</instanceType>
  <instanceState><name>stopped</name></instanceState>
  <privateIpAddress>192.0.2.11</privateIpAddress><platform>windows</platform>
</item></instancesSet></item></reservationSet></DescribeInstancesResponse>`

func TestInstancesPagesAndSigns(t *testing.T) {
	c := testClient(t)
	got := fakeAWS(t, map[string][]string{"DescribeInstances": {instancesPage1, instancesPage2}})
	is, err := c.Instances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(is) != 2 {
		t.Fatalf("instances = %+v", is)
	}
	a, b := is[0], is[1]
	if a.ID != "i-0123456789abcdef0" || a.Name != "web-1" || a.State != "running" || a.Type != "t3.micro" ||
		a.PrivateIP != "192.0.2.10" || a.PublicIP != "198.51.100.7" || a.KeyName != "sample-key" {
		t.Errorf("first = %+v", a)
	}
	if b.Name != "" || b.State != "stopped" || b.Platform != "windows" {
		t.Errorf("second = %+v", b)
	}
	reqs := *got
	if len(reqs) != 2 || reqs[0].PostForm.Get("NextToken") != "" || reqs[1].PostForm.Get("NextToken") != "page-2" {
		t.Fatalf("paging forms: %v", reqs)
	}
	auth := reqs[0].Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") || !strings.Contains(auth, "/us-east-1/ec2/aws4_request") {
		t.Errorf("not signed for ec2 in us-east-1: %q", auth)
	}
	if strings.Contains(auth, "sample-secret-key") {
		t.Error("the secret key is in the request")
	}
}

func TestAPIErrors(t *testing.T) {
	c := testClient(t)
	fakeAWS(t, map[string][]string{
		"DescribeInstances":   {`!<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>You are not authorized.</Message></Error></Errors></Response>`},
		"DescribeDBInstances": {`!<ErrorResponse><Error><Code>AccessDenied</Code><Message>Denied for rds.</Message></Error></ErrorResponse>`},
	})
	_, err := c.Instances(context.Background())
	if err == nil || !strings.Contains(err.Error(), "UnauthorizedOperation") || !strings.Contains(err.Error(), "You are not authorized.") {
		t.Errorf("ec2 error = %v", err)
	}
	_, err = c.Databases(context.Background())
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || !strings.Contains(err.Error(), "Denied for rds.") {
		t.Errorf("rds error = %v", err)
	}
}

func TestRegionsFallBack(t *testing.T) {
	c := testClient(t)
	fakeAWS(t, map[string][]string{"DescribeRegions": {
		`<DescribeRegionsResponse><regionInfo><item><regionName>us-east-1</regionName></item><item><regionName>ap-southeast-1</regionName></item></regionInfo></DescribeRegionsResponse>`,
		`!<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>no</Message></Error></Errors></Response>`,
	}})
	rs, checked, err := c.Regions(context.Background())
	if err != nil || !checked || !slices.Equal(rs, []string{"ap-southeast-1", "us-east-1"}) {
		t.Fatalf("regions = %v %v %v", rs, checked, err)
	}
	rs, checked, err = c.Regions(context.Background())
	if err != nil || checked || !slices.Contains(rs, "us-east-1") || !slices.Contains(rs, "eu-west-1") {
		t.Fatalf("fallback = %v %v %v", rs, checked, err)
	}
}

func TestIdentityAndInstanceAction(t *testing.T) {
	c := testClient(t)
	got := fakeAWS(t, map[string][]string{
		"GetCallerIdentity": {`<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account><Arn>arn:aws:sts::123456789012:assumed-role/Sample/user</Arn></GetCallerIdentityResult></GetCallerIdentityResponse>`},
		"StopInstances":     {`<StopInstancesResponse/>`},
		"StartInstances":    {`<StartInstancesResponse/>`},
		"RebootInstances":   {`<RebootInstancesResponse/>`},
	})
	id, err := c.Identity(context.Background())
	if err != nil || id.Account != "123456789012" || !strings.HasSuffix(id.ARN, "Sample/user") {
		t.Fatalf("identity = %+v %v", id, err)
	}
	if err := c.InstanceAction(context.Background(), "i-0123456789abcdef0", "stop"); err != nil {
		t.Fatal(err)
	}
	last := (*got)[len(*got)-1]
	if last.PostForm.Get("InstanceId.1") != "i-0123456789abcdef0" || !strings.HasSuffix(last.URL.Path, "/ec2/us-east-1") {
		t.Errorf("stop request: %v %s", last.PostForm, last.URL.Path)
	}
	for action, api := range map[string]string{"start": "StartInstances", "reboot": "RebootInstances"} {
		if err := c.InstanceAction(context.Background(), "i-0123456789abcdef0", action); err != nil {
			t.Fatal(action, err)
		}
		if last := (*got)[len(*got)-1]; last.PostForm.Get("Action") != api || last.PostForm.Get("InstanceId.1") != "i-0123456789abcdef0" {
			t.Errorf("%s request: %v", action, last.PostForm)
		}
	}
	if err := c.InstanceAction(context.Background(), "i-0123456789abcdef0", "terminate"); err == nil {
		t.Error("terminate must be refused")
	}
}

func TestDatabases(t *testing.T) {
	c := testClient(t)
	got := fakeAWS(t, map[string][]string{
		"DescribeDBInstances": {
			`<DescribeDBInstancesResponse><DescribeDBInstancesResult><DBInstances>
			<DBInstance><DBInstanceIdentifier>orders</DBInstanceIdentifier><Engine>postgres</Engine><DBInstanceStatus>available</DBInstanceStatus>
			<Endpoint><Address>orders.abc.us-east-1.rds.example.test</Address><Port>5432</Port></Endpoint><MasterUsername>admin</MasterUsername><DBName>orders</DBName></DBInstance>
			</DBInstances><Marker>m2</Marker></DescribeDBInstancesResult></DescribeDBInstancesResponse>`,
			`<DescribeDBInstancesResponse><DescribeDBInstancesResult><DBInstances>
			<DBInstance><DBInstanceIdentifier>db1</DBInstanceIdentifier><Engine>aurora-postgresql</Engine><DBInstanceStatus>available</DBInstanceStatus>
			<Endpoint><Address>db1.abc.us-east-1.rds.example.test</Address><Port>5432</Port></Endpoint><DBClusterIdentifier>main</DBClusterIdentifier></DBInstance>
			<DBInstance><DBInstanceIdentifier>legacy</DBInstanceIdentifier><Engine>db2-se</Engine><DBInstanceStatus>stopped</DBInstanceStatus></DBInstance>
			</DBInstances></DescribeDBInstancesResult></DescribeDBInstancesResponse>`,
		},
		"DescribeDBClusters": {`<DescribeDBClustersResponse><DescribeDBClustersResult><DBClusters>
			<DBCluster><DBClusterIdentifier>main</DBClusterIdentifier><Endpoint>main.cluster-abc.us-east-1.rds.example.test</Endpoint><Port>5432</Port>
			<MasterUsername>clusteradmin</MasterUsername><DatabaseName>app</DatabaseName></DBCluster>
			</DBClusters></DescribeDBClustersResult></DescribeDBClustersResponse>`},
	})
	dbs, err := c.Databases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dbs) != 3 {
		t.Fatalf("databases = %+v", dbs)
	}
	o, a, l := dbs[0], dbs[1], dbs[2]
	if o.Kind != "postgres" || o.SaveHost != o.Address || o.Port != 5432 || o.User != "admin" || o.DBName != "orders" {
		t.Errorf("orders = %+v", o)
	}
	if a.SaveHost != "main.cluster-abc.us-east-1.rds.example.test" || a.Address != "db1.abc.us-east-1.rds.example.test" ||
		a.User != "clusteradmin" || a.DBName != "app" || a.Kind != "postgres" {
		t.Errorf("aurora member = %+v", a)
	}
	if l.Kind != "" {
		t.Errorf("db2 has no Hostbay kind: %+v", l)
	}
	if o.NoClusterEndpoint || a.NoClusterEndpoint {
		t.Errorf("a found cluster endpoint is no hint: %+v %+v", o, a)
	}
	if (*got)[1].PostForm.Get("Marker") != "m2" || !strings.Contains((*got)[0].Header.Get("Authorization"), "/rds/aws4_request") {
		t.Error("rds paging or signing is wrong")
	}
}

// A role that may not list clusters still lists their members: each keeps
// its own address and says the cluster endpoint was not found, so the tab
// can warn before a saved host breaks at the next failover.
func TestDatabasesWithoutClusterList(t *testing.T) {
	c := testClient(t)
	fakeAWS(t, map[string][]string{
		"DescribeDBInstances": {`<DescribeDBInstancesResponse><DescribeDBInstancesResult><DBInstances>
			<DBInstance><DBInstanceIdentifier>db1</DBInstanceIdentifier><Engine>aurora-mysql</Engine><DBInstanceStatus>available</DBInstanceStatus>
			<Endpoint><Address>db1.abc.us-east-1.rds.example.test</Address><Port>3306</Port></Endpoint><DBClusterIdentifier>main</DBClusterIdentifier></DBInstance>
			<DBInstance><DBInstanceIdentifier>solo</DBInstanceIdentifier><Engine>mysql</Engine><DBInstanceStatus>available</DBInstanceStatus>
			<Endpoint><Address>solo.abc.us-east-1.rds.example.test</Address><Port>3306</Port></Endpoint></DBInstance>
			</DBInstances></DescribeDBInstancesResult></DescribeDBInstancesResponse>`},
		"DescribeDBClusters": {`!<ErrorResponse><Error><Code>AccessDenied</Code><Message>not allowed</Message></Error></ErrorResponse>`},
	})
	dbs, err := c.Databases(context.Background())
	if err != nil || len(dbs) != 2 {
		t.Fatalf("databases = %+v, %v", dbs, err)
	}
	m, s := dbs[0], dbs[1]
	if m.SaveHost != m.Address || !m.NoClusterEndpoint {
		t.Errorf("member without the cluster list = %+v", m)
	}
	if s.NoClusterEndpoint {
		t.Errorf("an instance outside a cluster needs no cluster endpoint: %+v", s)
	}
}

func TestDatabaseKind(t *testing.T) {
	for engine, want := range map[string]string{
		"postgres": "postgres", "aurora-postgresql": "postgres",
		"mysql": "mysql", "mariadb": "mysql", "aurora": "mysql", "aurora-mysql": "mysql",
		"sqlserver-ex": "mssql", "custom-sqlserver-ee": "mssql",
		"oracle-ee": "oracle", "custom-oracle-ee": "oracle",
		"db2-se": "", "docdb": "", "": "",
	} {
		if got := DatabaseKind(engine); got != want {
			t.Errorf("DatabaseKind(%q) = %q, want %q", engine, got, want)
		}
	}
}

func TestValidation(t *testing.T) {
	for id, ok := range map[string]bool{"i-0123456789abcdef0": true, "i-12345678": true, "-i-1": false, "i-XYZ": false, "i-0123456789abcdef0 ": false, "": false} {
		if ValidInstanceID(id) != ok {
			t.Errorf("ValidInstanceID(%q) != %v", id, ok)
		}
	}
	for r, ok := range map[string]bool{"us-east-1": true, "ap-southeast-1": true, "us-gov-west-1": true, "cn-north-1": true, "us-east-1;x": false, "--region": false, "": false} {
		if ValidRegion(r) != ok {
			t.Errorf("ValidRegion(%q) != %v", r, ok)
		}
	}
}

func TestEndpoint(t *testing.T) {
	if e := endpoint("ec2", "eu-west-1"); e != "https://ec2.eu-west-1.amazonaws.com/" {
		t.Error(e)
	}
	if e := endpoint("rds", "cn-north-1"); e != "https://rds.cn-north-1.amazonaws.com.cn/" {
		t.Error(e)
	}
}

func TestCLIArgs(t *testing.T) {
	if got := ShellArgs("dev", "us-east-1", "i-0123456789abcdef0"); !slices.Equal(got, []string{"ssm", "start-session", "--target", "i-0123456789abcdef0", "--profile", "dev", "--region", "us-east-1"}) {
		t.Error(got)
	}
	if got := LoginArgs("sso-base"); !slices.Equal(got, []string{"sso", "login", "--profile", "sso-base"}) {
		t.Error(got)
	}
}

// One refresh of the tab makes several calls, some in parallel; a profile's
// credential_process (which may prompt) must run once for all of them, and
// again only once the AWS files change.
func TestCredentialsLoadOncePerProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sample credential_process is a POSIX shell command")
	}
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	cfg := "[profile proc]\nregion = us-east-1\ncredential_process = sh -c 'echo run >> " + runs +
		"; printf \"{\\\"Version\\\": 1, \\\"AccessKeyId\\\": \\\"AKIDSAMPLE\\\", \\\"SecretAccessKey\\\": \\\"sample-secret-key\\\"}\"'\n"
	home := isolate(t, cfg, "")
	fakeAWS(t, map[string][]string{"GetCallerIdentity": {`<GetCallerIdentityResponse><GetCallerIdentityResult><Account>123456789012</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`}})
	count := func() int {
		b, _ := os.ReadFile(runs)
		return strings.Count(string(b), "run")
	}
	call := func() {
		t.Helper()
		c, err := New(context.Background(), "proc", "us-east-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Identity(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	call()
	call()
	call()
	if n := count(); n != 1 {
		t.Fatalf("credential_process ran %d times for one profile, want 1", n)
	}
	// Editing the config loads the profile afresh.
	conf := filepath.Join(home, "config")
	if err := os.WriteFile(conf, []byte(cfg+"\n# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	os.Chtimes(conf, later, later)
	call()
	if n := count(); n != 2 {
		t.Fatalf("after a config edit credential_process ran %d times in all, want 2", n)
	}
}

// A profile's setup is not kept past its time: the next load of any profile
// drops it, so its credentials do not stay in memory until Hostbay quits.
func TestCredentialsExpireFromMemory(t *testing.T) {
	isolate(t, "[profile a]\nregion = us-east-1\n[profile b]\nregion = us-east-1\n",
		"[a]\naws_access_key_id = AKIDSAMPLE\naws_secret_access_key = sample-secret-key\n[b]\naws_access_key_id = AKIDSAMPLE\naws_secret_access_key = sample-secret-key\n")
	if _, err := New(context.Background(), "a", "us-east-1"); err != nil {
		t.Fatal(err)
	}
	credsMu.Lock()
	old := credsCache["a\x00us-east-1"]
	old.at = time.Now().Add(-credentialsTTL)
	credsCache["a\x00us-east-1"] = old
	credsMu.Unlock()
	if _, err := New(context.Background(), "b", "us-east-1"); err != nil {
		t.Fatal(err)
	}
	credsMu.Lock()
	_, kept := credsCache["a\x00us-east-1"]
	credsMu.Unlock()
	if kept {
		t.Error("an expired profile setup is still held")
	}
}
