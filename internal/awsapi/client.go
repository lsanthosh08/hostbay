package awsapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
)

// endpoint is a service's Query API URL in a region; tests point it at a
// local server.
var endpoint = func(service, region string) string {
	host := service + "." + region + ".amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		host += ".cn"
	}
	return "https://" + host + "/"
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// maxBody caps a reply; a full page of instances is well under this.
const maxBody = 32 << 20

// Client calls AWS as one profile in one region. It holds the credentials
// provider, never the credentials.
type Client struct {
	creds  aws.CredentialsProvider
	region string
}

// CredentialsError is a failure to load or refresh a profile's credentials,
// as opposed to AWS refusing a call. Its text comes from the SDK and may
// quote what a credential_process printed, so callers decide what to show.
type CredentialsError struct{ Err error }

func (e *CredentialsError) Error() string { return e.Err.Error() }
func (e *CredentialsError) Unwrap() error { return e.Err }

// APIError is AWS refusing a call, with its error code and message.
type APIError struct {
	Code, Message string
	Status        int
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("AWS answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("AWS refused the request: %s: %s", e.Code, e.Message)
}

// Credentials setups are kept briefly per profile and region, so one refresh
// of the tab (account, regions and a list, some at once) shares one provider
// and runs a credential_process or SSO exchange once instead of once per
// call. An edit to either AWS file, or credentialsTTL passing, loads the
// profile afresh. Only the provider is kept; it never caches a failure.
const credentialsTTL = 5 * time.Minute

type cachedCreds struct {
	files    string
	provider aws.CredentialsProvider
	at       time.Time
}

var (
	credsMu    sync.Mutex
	credsCache = map[string]cachedCreds{}
)

// New gives a client for calls in region as profile. The EC2 metadata
// service is never asked: on a laptop it only times out.
func New(ctx context.Context, profile, region string) (*Client, error) {
	creds, err := credentials(ctx, profile, region)
	if err != nil {
		return nil, err
	}
	return &Client{creds: creds, region: region}, nil
}

func credentials(ctx context.Context, profile, region string) (aws.CredentialsProvider, error) {
	key, files := profile+"\x00"+region, filesStamp()
	credsMu.Lock()
	defer credsMu.Unlock()
	if c, ok := credsCache[key]; ok && c.files == files && time.Since(c.at) < credentialsTTL {
		return c.provider, nil
	}
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
		config.WithEC2IMDSClientEnableState(imds.ClientDisabled),
		config.WithHTTPClient(awshttp.NewBuildableClient().WithTimeout(httpClient.Timeout)),
	)
	if err != nil {
		return nil, &CredentialsError{err}
	}
	if cfg.Credentials == nil {
		return nil, &CredentialsError{errors.New("this profile has no credentials")}
	}
	credsCache[key] = cachedCreds{files: files, provider: cfg.Credentials, at: time.Now()}
	return cfg.Credentials, nil
}

// filesStamp names the AWS config and credentials files with their sizes
// and change times, so a cached setup is dropped when either changes.
func filesStamp() string {
	var b strings.Builder
	credentialsFile, configFile := configFiles()
	for _, p := range []string{credentialsFile, configFile} {
		fmt.Fprint(&b, p)
		if fi, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, ":%d:%d", fi.Size(), fi.ModTime().UnixNano())
		}
		b.WriteByte(0)
	}
	return b.String()
}

// Identity is the account and caller the credentials belong to.
type Identity struct {
	Account string `json:"account"`
	ARN     string `json:"arn"`
}

func (c *Client) Identity(ctx context.Context) (Identity, error) {
	var r struct {
		Account string `xml:"GetCallerIdentityResult>Account"`
		ARN     string `xml:"GetCallerIdentityResult>Arn"`
	}
	err := c.query(ctx, "sts", "2011-06-15", "GetCallerIdentity", nil, &r)
	return Identity{Account: r.Account, ARN: r.ARN}, err
}

// query makes one signed Query API call and decodes the XML reply into out.
func (c *Client) query(ctx context.Context, service, version, action string, params url.Values, out any) error {
	creds, err := c.creds.Retrieve(ctx)
	if err != nil {
		return &CredentialsError{err}
	}
	form := url.Values{"Action": {action}, "Version": {version}}
	for k, v := range params {
		form[k] = v
	}
	body := form.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(service, c.region), strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	sum := sha256.Sum256([]byte(body))
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), service, c.region, time.Now()); err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		// EC2 nests its errors under Errors; RDS and STS do not.
		var e struct {
			EC2 struct{ Code, Message string } `xml:"Errors>Error"`
			Std struct{ Code, Message string } `xml:"Error"`
		}
		xml.Unmarshal(data, &e)
		ae := &APIError{Code: e.EC2.Code, Message: e.EC2.Message, Status: resp.StatusCode}
		if ae.Code == "" {
			ae.Code, ae.Message = e.Std.Code, e.Std.Message
		}
		return ae
	}
	if err := xml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("AWS sent a reply Hostbay could not read: %w", err)
	}
	return nil
}
