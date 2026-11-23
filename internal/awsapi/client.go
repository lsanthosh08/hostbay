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
	"strings"
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

// New loads profile's credentials setup for calls in region. The EC2
// metadata service is never asked: on a laptop it only times out.
func New(ctx context.Context, profile, region string) (*Client, error) {
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
	return &Client{creds: cfg.Credentials, region: region}, nil
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
