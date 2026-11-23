package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"hostbay/internal/apperr"
	"hostbay/internal/awsapi"
)

// awsFiles points the AWS config files and home folder at a temp dir, so
// no test reads this computer's own AWS setup.
func awsFiles(t *testing.T, config string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, k := range []string{"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

const awsTestConfig = `
[profile sso-base]
sso_session = corp
sso_account_id = 123456789012
sso_role_name = Sample
region = us-east-1

[sso-session corp]
sso_start_url = https://sample.example.test/start
sso_region = us-east-1

[profile role]
source_profile = sso-base
role_arn = arn:aws:iam::123456789012:role/sample
region = us-east-1

[profile mfa]
role_arn = arn:aws:iam::123456789012:role/admin
mfa_serial = arn:aws:iam::123456789012:mfa/sample
credential_source = Environment

[profile proc]
region = us-east-1
credential_process = printf '{"Version": "one", "AccessKeyId": "AKIDSAMPLE", "SecretAccessKey": "sample-secret-key"}'

[profile --debug]
region = us-east-1
`

func TestAwsRefusesBadInputs(t *testing.T) {
	awsFiles(t, awsTestConfig)
	s := &AwsService{}
	ctx := context.Background()
	if _, err := s.AwsInstances(ctx, "missing", "us-east-1"); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("a profile not in the config files: %v", err)
	}
	if _, err := s.AwsInstances(ctx, "sso-base", "us-east-1;x"); err == nil {
		t.Error("a malformed region must be refused")
	}
	if err := s.AwsInstanceAction(ctx, "sso-base", "us-east-1", "i-0123456789abcdef0", "terminate"); err == nil {
		t.Error("terminate must be refused")
	}
	if err := s.AwsInstanceAction(ctx, "sso-base", "us-east-1", "-i-1", "stop"); err == nil {
		t.Error("a malformed instance ID must be refused")
	}
	if _, err := s.AwsShell(ctx, "owner", "--debug", "us-east-1", "i-0123456789abcdef0", 80, 24); err == nil || !strings.Contains(err.Error(), "--debug") {
		t.Errorf("a profile named like a flag must be refused: %v", err)
	}
	if _, err := s.AwsSignIn(ctx, "owner", "--debug", 80, 24); err == nil {
		t.Error("a profile named like a flag must be refused for sign-in")
	}
}

func TestAwsSignInNeeded(t *testing.T) {
	awsFiles(t, awsTestConfig)
	s := &AwsService{}
	for _, profile := range []string{"sso-base", "role"} {
		_, err := s.AwsAccount(context.Background(), profile, "us-east-1")
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code != apperr.AWSSignIn || ae.Details["profile"] != "sso-base" {
			t.Errorf("%s: want AWSSignIn naming sso-base, got %v", profile, err)
		}
	}
}

func TestAwsMFAProfileRefused(t *testing.T) {
	awsFiles(t, awsTestConfig)
	_, err := (&AwsService{}).AwsAccount(context.Background(), "mfa", "us-east-1")
	if err == nil || !strings.Contains(err.Error(), "MFA") {
		t.Fatalf("want the MFA message, got %v", err)
	}
}

func TestAwsCredentialProcessOutputNotShown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sample credential_process is a POSIX shell command")
	}
	awsFiles(t, awsTestConfig)
	_, err := (&AwsService{}).AwsAccount(context.Background(), "proc", "us-east-1")
	if err == nil {
		t.Fatal("malformed credential_process output must fail")
	}
	if strings.Contains(err.Error(), "sample-secret-key") || strings.Contains(err.Error(), "AKIDSAMPLE") {
		t.Fatalf("the error repeats the process output: %v", err)
	}
	if !strings.Contains(err.Error(), "proc") {
		t.Errorf("the error should name the profile: %v", err)
	}
}

func TestAwsSignInUsesSSORoot(t *testing.T) {
	awsFiles(t, awsTestConfig)
	p, err := awsProfile("role")
	if err != nil {
		t.Fatal(err)
	}
	args, err := signInArgs(p)
	if err != nil || strings.Join(args, " ") != "sso login --profile sso-base" {
		t.Fatalf("args = %v %v", args, err)
	}
	p, _ = awsProfile("proc")
	if _, err := signInArgs(p); err == nil {
		t.Error("a profile without SSO has nothing to sign in to")
	}
}

func TestAwsMissingCLI(t *testing.T) {
	t.Setenv("PATH", "")
	if _, err := awsapi.FindCLI(); err == nil {
		t.Skip("the AWS CLI is in an install folder on this computer")
	}
	awsFiles(t, awsTestConfig)
	_, err := (&AwsService{}).AwsSignIn(context.Background(), "owner", "sso-base", 80, 24)
	if err == nil || !strings.Contains(err.Error(), "AWS CLI") {
		t.Fatalf("want an error naming the AWS CLI, got %v", err)
	}
}
