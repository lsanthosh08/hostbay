// Package awsapi reads the AWS profiles on this computer and makes the few
// signed AWS API calls Hostbay's AWS tab needs. Credentials come from the
// AWS config the CLI already uses; Hostbay stores none and never returns
// one to the page.
package awsapi

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ssocreds"
)

// Profile is a named profile in the AWS config or credentials file.
// SSORoot is the profile holding the SSO settings its credentials come
// from: itself, or the first one up its source_profile chain. MFA is set
// when it or that chain needs an MFA code, which Hostbay does not ask for.
type Profile struct {
	Name    string `json:"name"`
	Region  string `json:"region,omitempty"`
	SSORoot string `json:"ssoRoot,omitempty"`
	MFA     bool   `json:"mfa,omitempty"`

	ssoKey string // the SSO token cache key: the sso_session name, else sso_start_url
}

// Profiles lists the profiles in the config and credentials files the SDK
// reads (AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE when set), the
// default profile first and the rest by name. A missing file is no error.
func Profiles() ([]Profile, error) {
	sections := map[string]map[string]string{}
	add := func(path string, configFile bool) error {
		secs, err := readINI(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for name, keys := range secs {
			if configFile {
				// The config file names profiles "profile x", except "default";
				// other sections (sso-session, services) are not profiles.
				n, ok := strings.CutPrefix(name, "profile ")
				if !ok && name != "default" {
					continue
				}
				name = strings.TrimSpace(n)
			}
			if sections[name] == nil {
				sections[name] = map[string]string{}
			}
			for k, v := range keys {
				if _, seen := sections[name][k]; !seen || configFile {
					sections[name][k] = v
				}
			}
		}
		return nil
	}
	credentialsFile, configFile := configFiles()
	if err := add(credentialsFile, false); err != nil {
		return nil, err
	}
	if err := add(configFile, true); err != nil {
		return nil, err
	}

	var out []Profile
	for name, keys := range sections {
		p := Profile{Name: name, Region: keys["region"]}
		// Follow source_profile to the SSO settings; a loop just stops.
		seen := map[string]bool{}
		for cur := name; cur != "" && !seen[cur]; cur = sections[cur]["source_profile"] {
			seen[cur] = true
			k := sections[cur]
			if k == nil {
				break
			}
			// The SDK stops at a source profile with its own keys, and only
			// assuming a role (role_arn) asks for the mfa_serial code.
			if cur != name && k["aws_access_key_id"] != "" {
				break
			}
			p.MFA = p.MFA || (k["mfa_serial"] != "" && k["role_arn"] != "")
			if key := cmp.Or(k["sso_session"], k["sso_start_url"]); key != "" {
				p.SSORoot, p.ssoKey = cur, key
				break
			}
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Profile) int {
		switch {
		case a.Name == "default":
			return -1
		case b.Name == "default":
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// SignInNeeded reports whether p's SSO sign-in is missing or expired, by
// the token cache file the SDK and CLI both use, and the expiry it records.
// A profile without SSO never needs one.
func SignInNeeded(p Profile) (bool, string) {
	if p.ssoKey == "" {
		return false, ""
	}
	path, err := cachedTokenPath(p.ssoKey)
	if err != nil {
		return true, ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return true, ""
	}
	var tok struct {
		ExpiresAt string `json:"expiresAt"`
	}
	if json.Unmarshal(b, &tok) != nil {
		return true, ""
	}
	at, err := time.Parse(time.RFC3339, tok.ExpiresAt)
	return err != nil || !at.After(time.Now()), tok.ExpiresAt
}

// configFiles is the credentials and config file the SDK reads.
func configFiles() (credentialsFile, configFile string) {
	return cmp.Or(os.Getenv("AWS_SHARED_CREDENTIALS_FILE"), config.DefaultSharedCredentialsFilename()),
		cmp.Or(os.Getenv("AWS_CONFIG_FILE"), config.DefaultSharedConfigFilename())
}

func cachedTokenPath(key string) (string, error) { return ssocreds.StandardCachedTokenFilepath(key) }

// readINI reads an AWS-style INI file into sections of keys. Indented lines
// are nested settings (such as an "s3 =" block) and are skipped.
func readINI(path string) (map[string]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]map[string]string{}
	var cur map[string]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[' && strings.HasSuffix(line, "]"):
			name := strings.TrimSpace(line[1 : len(line)-1])
			if out[name] == nil {
				out[name] = map[string]string{}
			}
			cur = out[name]
		case cur != nil && raw[0] != ' ' && raw[0] != '\t':
			if k, v, ok := strings.Cut(line, "="); ok {
				cur[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	return out, sc.Err()
}
