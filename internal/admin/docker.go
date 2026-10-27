package admin

import (
	"context"
	"path"
	"strings"

	"golang.org/x/crypto/ssh"

	"hostbay/internal/sftp"
)

// Ported from docker.rs. The engine commands themselves moved to
// internal/container (KTD27); what stays here is shell text for a remote
// host only.

type ComposeProject struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// ComposeProjects finds compose files up to three levels under ~, /opt,
// /srv and extraPaths. docker.rs pasted extra paths into the script
// unquoted, so "$(...)" in one ran; here each is quoted.
// shortcut: a quoted extra path gets no ~ expansion; the UI passes none.
func ComposeProjects(ctx context.Context, c *ssh.Client, extraPaths []string) ([]ComposeProject, error) {
	dirs := "~ /opt /srv"
	for _, p := range extraPaths {
		if err := ValidComposePath(p); err != nil {
			return nil, err
		}
		dirs += " " + sftp.Quote(p)
	}
	script := "for d in " + dirs + `; do find "$d" -maxdepth 3 -name "docker-compose.y*ml" -o -name "compose.y*ml" 2>/dev/null; done`
	// Not an error on a non-zero exit: find exits so for a missing directory such as /srv.
	r, err := Exec(ctx, c, "sh -lc "+sftp.Quote(script))
	if err != nil {
		return nil, err
	}
	var list []ComposeProject
	seen := map[string]bool{}
	for _, p := range strings.Split(r.Stdout, "\n") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		list = append(list, ComposeProject{Name: path.Base(path.Dir(p)), Path: p})
	}
	return list, nil
}

func DockerFixPermission(ctx context.Context, c *ssh.Client) (Result, error) {
	return Exec(ctx, c, "sudo usermod -aG docker $USER")
}
