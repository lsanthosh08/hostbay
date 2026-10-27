package container

import (
	"slices"
	"testing"
)

func TestCreateArgs(t *testing.T) {
	e, _ := engine(t, Docker, "", nil)
	got, err := e.CreateArgs(CreateOptions{
		Image: "nginx:1.27",
		Name:  "web",
		Ports: []PortOption{
			{HostPort: 8080, ContainerPort: 80},
			{HostAddress: "::1", HostPort: 5353, ContainerPort: 53, Protocol: "udp"},
			{HostAddress: "0.0.0.0", HostPort: 8443, ContainerPort: 443},
		},
		Env:     []string{"MODE=prod", "EMPTY="},
		Volumes: []MountOption{{Source: "webdata", Target: "/data"}},
		Binds:   []MountOption{{Source: "/srv/site", Target: "/usr/share/nginx/html", ReadOnly: true}},
		Restart: "unless-stopped",
	})
	want := []string{"docker", "run", "-d", "--name", "web",
		"-p", "127.0.0.1:8080:80", "-p", "[::1]:5353:53/udp", "-p", "0.0.0.0:8443:443",
		"-e", "MODE=prod", "-e", "EMPTY=",
		"-v", "webdata:/data", "-v", "/srv/site:/usr/share/nginx/html:ro",
		"--restart", "unless-stopped", "--", "nginx:1.27"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("args = %q, %v\nwant %q", got, err, want)
	}
}

func TestCreateRefuses(t *testing.T) {
	d, _ := engine(t, Docker, "", nil)
	w, _ := engine(t, WSLC, "", nil)
	for name, c := range map[string]struct {
		e *Engine
		o CreateOptions
	}{
		"no image":          {d, CreateOptions{}},
		"image option":      {d, CreateOptions{Image: "--privileged"}},
		"port address":      {d, CreateOptions{Image: "a", Ports: []PortOption{{HostAddress: "0.0.0.0:1", HostPort: 1, ContainerPort: 1}}}},
		"port range":        {d, CreateOptions{Image: "a", Ports: []PortOption{{HostPort: 70000, ContainerPort: 1}}}},
		"protocol":          {d, CreateOptions{Image: "a", Ports: []PortOption{{HostPort: 1, ContainerPort: 1, Protocol: "sctp;x"}}}},
		"env key":           {d, CreateOptions{Image: "a", Env: []string{"A B=1"}}},
		"env line break":    {d, CreateOptions{Image: "a", Env: []string{"A=1\n-v=/:/x"}}},
		"env without value": {d, CreateOptions{Image: "a", Env: []string{"AWS_SECRET_ACCESS_KEY"}}},
		"volume name":       {d, CreateOptions{Image: "a", Volumes: []MountOption{{Source: "/etc", Target: "/x"}}}},
		"volume dot":        {d, CreateOptions{Image: "a", Volumes: []MountOption{{Source: ".", Target: "/x"}}}},
		"volume dot dot":    {d, CreateOptions{Image: "a", Volumes: []MountOption{{Source: "..", Target: "/x"}}}},
		"volume dot name":   {d, CreateOptions{Image: "a", Volumes: []MountOption{{Source: ".ssh", Target: "/x"}}}},
		"relative bind":     {d, CreateOptions{Image: "a", Binds: []MountOption{{Source: "etc", Target: "/x"}}}},
		"bind with a colon": {d, CreateOptions{Image: "a", Binds: []MountOption{{Source: "/a:/b", Target: "/x"}}}},
		"target with comma": {d, CreateOptions{Image: "a", Volumes: []MountOption{{Source: "v", Target: "/x,ro"}}}},
		"restart policy":    {d, CreateOptions{Image: "a", Restart: "sometimes"}},
		"wslc restart":      {w, CreateOptions{Image: "a", Restart: "always"}},
	} {
		if args, err := c.e.CreateArgs(c.o); err == nil {
			t.Errorf("%s: accepted as %q", name, args)
		}
	}
	if args, err := d.CreateArgs(CreateOptions{Image: "a", Binds: []MountOption{{Source: `C:\data`, Target: "/data"}}}); err != nil || args[4] != `C:\data:/data` {
		t.Errorf("Windows folder = %q, %v", args, err)
	}
}
