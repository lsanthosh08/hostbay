package awsapi

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"hostbay/internal/container"
)

// installDirs are where the AWS CLI and the Session Manager plugin install,
// searched after PATH because an app started from the macOS Finder or a
// desktop menu gets a short PATH.
var installDirs = map[string][]string{
	"darwin":  {"/usr/local/bin", "/opt/homebrew/bin", "/usr/local/aws-cli", "/usr/local/sessionmanagerplugin/bin", "~/.local/bin"},
	"linux":   {"/usr/local/bin", "~/.local/bin", "/usr/local/sessionmanagerplugin/bin"},
	"windows": {`%ProgramFiles%\Amazon\AWSCLIV2`, `%ProgramFiles%\Amazon\SessionManagerPlugin\bin`},
}

// FindCLI finds the aws program.
func FindCLI() (string, error) { return container.FindBinary("aws", installDirs[runtime.GOOS]) }

// HasPlugin reports whether the Session Manager plugin, which aws ssm
// start-session runs, can be found.
func HasPlugin() bool {
	_, err := container.FindBinary("session-manager-plugin", installDirs[runtime.GOOS])
	return err == nil
}

// ExtendPath appends the install folders missing from this process's PATH,
// so a credential_process command the SDK runs, and the plugin the CLI
// runs, resolve when Hostbay was not started from a shell.
func ExtendPath() {
	have := filepath.SplitList(os.Getenv("PATH"))
	add := have
	for _, d := range container.ExpandDirs(installDirs[runtime.GOOS]) {
		if filepath.IsAbs(d) && !slices.Contains(add, d) {
			add = append(add, d)
		}
	}
	if len(add) > len(have) {
		os.Setenv("PATH", strings.Join(add, string(os.PathListSeparator)))
	}
}

// CLIEnv is this process's environment with the folder of the aws program
// first on PATH, so its plugin beside it resolves.
func CLIEnv(cli string) []string {
	env := []string{"PATH=" + filepath.Dir(cli) + string(os.PathListSeparator) + os.Getenv("PATH")}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "PATH") {
			env = append(env, kv)
		}
	}
	return env
}

// ShellArgs are the aws arguments for an SSM shell on an instance.
func ShellArgs(profile, region, instanceID string) []string {
	return []string{"ssm", "start-session", "--target", instanceID, "--profile", profile, "--region", region}
}

// LoginArgs are the aws arguments for an SSO sign-in of a profile.
func LoginArgs(profile string) []string { return []string{"sso", "login", "--profile", profile} }
