package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"

	"hostbay/internal/apperr"
	"hostbay/internal/awsapi"
	"hostbay/internal/terminal"
)

// AwsService serves the AWS tab: the profiles in this computer's AWS
// config, and per profile and region the account, EC2 instances and RDS
// databases. It holds no session: every call names its profile and
// region, and credentials load from the AWS config each time and never
// leave Go. SSO sign-in and SSM shells run the user's aws program in a
// terminal tab with an argument list Go builds.
type AwsService struct {
	conns *ConnService
}

func NewAwsService(conns *ConnService) *AwsService {
	awsapi.ExtendPath()
	return &AwsService{conns: conns}
}

// AwsTools says whether the AWS CLI and the Session Manager plugin were
// found, so the tab can say what to install before a shell fails.
type AwsTools struct {
	CLI    bool `json:"cli"`
	Plugin bool `json:"plugin"`
}

// AwsRegionList is the regions to offer; Checked is false when the
// account's own list could not be read and a built-in one stands in.
type AwsRegionList struct {
	Regions []string `json:"regions"`
	Checked bool     `json:"checked"`
}

func (*AwsService) AwsProfiles() ([]awsapi.Profile, error) { return awsapi.Profiles() }

func (*AwsService) AwsTools() AwsTools {
	_, err := awsapi.FindCLI()
	return AwsTools{CLI: err == nil, Plugin: awsapi.HasPlugin()}
}

func (s *AwsService) AwsAccount(ctx context.Context, profile, region string) (awsapi.Identity, error) {
	c, p, err := awsClient(ctx, profile, region)
	if err != nil {
		return awsapi.Identity{}, err
	}
	id, err := c.Identity(ctx)
	return id, awsErr(p, err)
}

func (s *AwsService) AwsRegions(ctx context.Context, profile, region string) (AwsRegionList, error) {
	c, p, err := awsClient(ctx, profile, region)
	if err != nil {
		return AwsRegionList{}, err
	}
	rs, checked, err := c.Regions(ctx)
	return AwsRegionList{Regions: rs, Checked: checked}, awsErr(p, err)
}

func (s *AwsService) AwsInstances(ctx context.Context, profile, region string) ([]awsapi.Instance, error) {
	c, p, err := awsClient(ctx, profile, region)
	if err != nil {
		return nil, err
	}
	is, err := c.Instances(ctx)
	return is, awsErr(p, err)
}

func (s *AwsService) AwsDatabases(ctx context.Context, profile, region string) ([]awsapi.Database, error) {
	c, p, err := awsClient(ctx, profile, region)
	if err != nil {
		return nil, err
	}
	dbs, err := c.Databases(ctx)
	return dbs, awsErr(p, err)
}

// AwsInstanceAction starts, stops or reboots an instance; the tab has
// already confirmed it with the user. Terminate is not offered.
func (s *AwsService) AwsInstanceAction(ctx context.Context, profile, region, id, action string) error {
	if action != "start" && action != "stop" && action != "reboot" {
		return errors.New("unknown instance action: " + action)
	}
	if !awsapi.ValidInstanceID(id) {
		return errors.New("not an EC2 instance ID: " + id)
	}
	c, p, err := awsClient(ctx, profile, region)
	if err != nil {
		return err
	}
	return awsErr(p, c.InstanceAction(ctx, id, action))
}

// AwsShell opens an SSM shell on an instance through the aws program, in a
// terminal owned by owner, and returns its ID.
func (s *AwsService) AwsShell(ctx context.Context, owner, profile, region, id string, cols, rows int) (string, error) {
	p, err := awsProfile(profile)
	if err != nil {
		return "", err
	}
	if !awsapi.ValidRegion(region) {
		return "", errors.New("not an AWS region: " + region)
	}
	if !awsapi.ValidInstanceID(id) {
		return "", errors.New("not an EC2 instance ID: " + id)
	}
	return s.runCLI(owner, awsapi.ShellArgs(p.Name, region, id), cols, rows)
}

// AwsSignIn runs aws sso login for the profile holding profile's SSO
// settings, in a terminal owned by owner, and returns its ID.
func (s *AwsService) AwsSignIn(ctx context.Context, owner, profile string, cols, rows int) (string, error) {
	p, err := awsProfile(profile)
	if err != nil {
		return "", err
	}
	args, err := signInArgs(p)
	if err != nil {
		return "", err
	}
	return s.runCLI(owner, args, cols, rows)
}

func signInArgs(p awsapi.Profile) ([]string, error) {
	if p.SSORoot == "" {
		return nil, fmt.Errorf("profile %s does not use AWS SSO", p.Name)
	}
	return awsapi.LoginArgs(p.SSORoot), nil
}

// runCLI runs the aws program with args, never through a shell.
func (s *AwsService) runCLI(owner string, args []string, cols, rows int) (string, error) {
	cli, err := awsapi.FindCLI()
	if err != nil {
		return "", errors.New("The AWS CLI was not found on this computer. Install AWS CLI version 2, then try again.")
	}
	t, err := terminal.OpenCommand(cli, args, awsapi.CLIEnv(cli), cols, rows)
	if err != nil {
		return "", err
	}
	return s.conns.terms.Open(owner, t), nil
}

// awsProfile finds a profile by name in the AWS config files. A name that
// looks like a flag is refused, since it is passed to the aws program.
func awsProfile(name string) (awsapi.Profile, error) {
	if name == "" || strings.HasPrefix(name, "-") {
		return awsapi.Profile{}, fmt.Errorf("%q is not a profile name Hostbay can use", name)
	}
	ps, err := awsapi.Profiles()
	if err != nil {
		return awsapi.Profile{}, err
	}
	for _, p := range ps {
		if p.Name == name {
			return p, nil
		}
	}
	return awsapi.Profile{}, fmt.Errorf("profile %s is not in the AWS config files", name)
}

func awsClient(ctx context.Context, profile, region string) (*awsapi.Client, awsapi.Profile, error) {
	p, err := awsProfile(profile)
	if err != nil {
		return nil, p, err
	}
	if !awsapi.ValidRegion(region) {
		return nil, p, errors.New("not an AWS region: " + region)
	}
	if p.MFA {
		return nil, p, mfaErr(p)
	}
	c, err := awsapi.New(ctx, p.Name, region)
	if err != nil {
		return nil, p, awsErr(p, err)
	}
	return c, p, nil
}

func mfaErr(p awsapi.Profile) error {
	return fmt.Errorf("Profile %s assumes a role that needs an MFA code, which Hostbay does not ask for yet.", p.Name)
}

// awsErr turns a credentials failure into what the tab can show and act
// on. An SSO sign-in that is missing or expired is AWSSignIn. A
// credential_process failure gets a fixed message, since the SDK's text
// can repeat what the process printed, secret key included. AWS refusing
// a call passes through.
func awsErr(p awsapi.Profile, err error) error {
	var ce *awsapi.CredentialsError
	if err == nil || !errors.As(err, &ce) {
		return err
	}
	var proc *processcreds.ProviderError
	var mfa config.AssumeRoleTokenProviderNotSetError
	switch {
	case errors.As(err, &proc):
		return fmt.Errorf("The credential_process of profile %s failed or printed something Hostbay could not read. Run it in a terminal to see why.", p.Name)
	case errors.As(err, &mfa):
		return mfaErr(p)
	}
	if need, at := awsapi.SignInNeeded(p); need {
		e := apperr.New(apperr.AWSSignIn, fmt.Sprintf("The AWS SSO sign-in for profile %s is missing or expired.", p.SSORoot)).With("profile", p.SSORoot)
		if at != "" {
			e = e.With("expiresAt", at)
		}
		return e
	}
	return fmt.Errorf("Profile %s: %v", p.Name, ce.Err)
}
