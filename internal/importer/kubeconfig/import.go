// Package kubeconfig imports the contexts of a kubeconfig file as
// Kubernetes hosts, once (R57, KTD28). Hostbay never reads the file live
// and never writes it. Contexts that sign in by running a program (exec
// plugins, auth providers), by sending a local file (tokenFile), with a
// user name and password, or as another user (impersonation) are refused
// with the reason in the report. Referenced certificate and key files are
// read once, capped at 64 KiB, and kept only when they parse as a CA, a
// certificate and its matching key; the stored host holds the data, so no
// path is ever followed later.
package kubeconfig

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"go.yaml.in/yaml/v3"

	"hostbay/internal/importer"
	"hostbay/internal/store"
)

type config struct {
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server        string `yaml:"server"`
			CAData        string `yaml:"certificate-authority-data"`
			CA            string `yaml:"certificate-authority"`
			TLSServerName string `yaml:"tls-server-name"`
			Insecure      bool   `yaml:"insecure-skip-tls-verify"`
			ProxyURL      string `yaml:"proxy-url"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token        string     `yaml:"token"`
			TokenFile    string     `yaml:"tokenFile"`
			CertData     string     `yaml:"client-certificate-data"`
			Cert         string     `yaml:"client-certificate"`
			KeyData      string     `yaml:"client-key-data"`
			Key          string     `yaml:"client-key"`
			Username     string     `yaml:"username"`
			Password     string     `yaml:"password"`
			As           string     `yaml:"as"`
			AsUID        string     `yaml:"as-uid"`
			AsGroups     []string   `yaml:"as-groups"`
			Exec         *yaml.Node `yaml:"exec"`
			AuthProvider *yaml.Node `yaml:"auth-provider"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

// maxRef caps a certificate or key file a kubeconfig points to.
const maxRef = 64 << 10

// readRef reads a file the kubeconfig names; tests watch it.
var readRef = func(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRef+1))
	if err == nil && len(b) > maxRef {
		return nil, fmt.Errorf("%s is over 64 KiB", path)
	}
	return b, err
}

var namespaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Parse reads the kubeconfig at path (default ~/.kube/config). password
// is unused: kubeconfigs are not encrypted.
func Parse(path, _ string) (importer.Result, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return importer.Result{}, err
		}
		path = filepath.Join(home, ".kube", "config")
	}
	raw, err := importer.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return importer.Result{}, fmt.Errorf("no kubeconfig at %s", path)
	}
	if err != nil {
		return importer.Result{}, err
	}
	var cfg config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return importer.Result{}, fmt.Errorf("%s is not a kubeconfig: %w", path, err)
	}
	dir := filepath.Dir(path)
	b := importer.NewBuilder("kubeconfig")
	for _, c := range cfg.Contexts {
		e, key, note, err := entry(&cfg, c.Name, c.Context.Cluster, c.Context.User, c.Context.Namespace, dir)
		if err != nil {
			b.Skip(c.Name, err.Error())
			continue
		}
		if key != nil {
			e.Host.IdentityFile = b.AddKey(c.Name, key)
		}
		b.Add(e)
		for _, n := range note {
			b.Note(c.Name, n)
		}
	}
	return b.Result(), nil
}

// entry maps one context, or says why it cannot be imported. key is the
// client certificate and key as one PEM, for the key store.
func entry(cfg *config, name, clusterName, userName, ns, dir string) (e importer.Entry, key []byte, notes []string, err error) {
	ci, ui := -1, -1
	for i := range cfg.Clusters {
		if cfg.Clusters[i].Name == clusterName {
			ci = i
		}
	}
	for i := range cfg.Users {
		if cfg.Users[i].Name == userName {
			ui = i
		}
	}
	if ci < 0 {
		return e, nil, nil, fmt.Errorf("its cluster %q is not in the file", clusterName)
	}
	if ui < 0 {
		return e, nil, nil, fmt.Errorf("its user %q is not in the file", userName)
	}
	cl, u := cfg.Clusters[ci].Cluster, cfg.Users[ui].User
	switch {
	case u.Exec != nil:
		return e, nil, nil, errors.New("it signs in through an exec plugin, which runs a program on this computer; Hostbay does not run it (EKS, GKE and AKS sign-in is not supported yet)")
	case u.AuthProvider != nil:
		return e, nil, nil, errors.New("it signs in through an auth provider, which Hostbay does not run")
	case u.TokenFile != "":
		return e, nil, nil, errors.New("it reads its token from a token file, which Hostbay does not open; put the token in the kubeconfig instead")
	case u.Username != "" || u.Password != "":
		return e, nil, nil, errors.New("it signs in with a user name and password, which Kubernetes no longer supports")
	case u.As != "" || u.AsUID != "" || len(u.AsGroups) > 0:
		return e, nil, nil, errors.New("it impersonates another user, which Hostbay does not support")
	}
	srv, perr := url.Parse(cl.Server)
	if perr != nil || srv.Scheme != "https" || srv.Hostname() == "" || srv.User != nil || srv.RawQuery != "" {
		return e, nil, nil, fmt.Errorf("its server %q is not an https:// URL", cl.Server)
	}
	port := 443
	if srv.Port() != "" {
		port, _ = strconv.Atoi(srv.Port())
	}
	h := store.Host{Alias: name, Hostname: srv.Hostname(), Port: port, Kind: new("kubernetes"), URL: new(srv.String()), Tags: []string{}}
	if ns != "" {
		if !namespaceRE.MatchString(ns) || len(ns) > 63 {
			return e, nil, nil, fmt.Errorf("its namespace %q is not a valid name", ns)
		}
		h.Namespace = new(ns)
	}
	if cl.TLSServerName != "" {
		h.TLSServerName = new(cl.TLSServerName)
	}
	ca, err := data("certificate authority", cl.CAData, cl.CA, dir)
	if err != nil {
		return e, nil, nil, err
	}
	if ca != nil {
		if !x509.NewCertPool().AppendCertsFromPEM(ca) {
			return e, nil, nil, errors.New("its certificate authority is not a certificate")
		}
		h.CAData = new(string(ca))
	}
	if cl.Insecure {
		notes = append(notes, "insecure-skip-tls-verify was not imported; Hostbay checks the server's certificate, and asks you to trust it on the first connect when it is self-signed")
	}
	if cl.ProxyURL != "" {
		h.ProxyID = importer.MissingProxy()
		notes = append(notes, "its proxy-url was not imported; choose a Hostbay proxy for it before connecting")
	}
	cert, err := data("client certificate", u.CertData, u.Cert, dir)
	if err != nil {
		return e, nil, nil, err
	}
	k, err := data("client key", u.KeyData, u.Key, dir)
	if err != nil {
		return e, nil, nil, err
	}
	switch {
	case cert == nil && k == nil:
	case cert == nil || k == nil:
		return e, nil, nil, errors.New("it has a client certificate or key without the other")
	default:
		if _, err := tls.X509KeyPair(cert, k); err != nil {
			return e, nil, nil, errors.New("its client key is not the private key of its client certificate")
		}
		key = append(append(cert, '\n'), k...)
	}
	return importer.Entry{Name: name, Host: h, Password: u.Token}, key, notes, nil
}

// data is inline base64 data, or the file at path (relative to the
// kubeconfig's folder), read once; nil when neither is set.
func data(what, inline, path, dir string) ([]byte, error) {
	if inline != "" {
		b, err := base64.StdEncoding.DecodeString(inline)
		if err != nil {
			return nil, fmt.Errorf("its %s data is not base64", what)
		}
		return b, nil
	}
	if path == "" {
		return nil, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	b, err := readRef(path)
	if err != nil {
		return nil, fmt.Errorf("its %s could not be read: %w", what, err)
	}
	if p, _ := pem.Decode(b); p == nil {
		return nil, fmt.Errorf("its %s file is not PEM", what)
	}
	return b, nil
}
