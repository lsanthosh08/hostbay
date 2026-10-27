package store

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/kevinburke/ssh_config"
)

type sshConfigHost struct {
	Host
	ProxyJump string
}

// parseSSHConfig reads each named Host block's own settings. Like SSH
// Manager it takes the block's first alias, skips wildcard and negated
// blocks, and ignores defaults inherited from them.
func parseSSHConfig(r io.Reader) ([]sshConfigHost, error) {
	cfg, err := ssh_config.Decode(r)
	if err != nil {
		return nil, err
	}
	var out []sshConfigHost
	for _, block := range cfg.Hosts {
		if len(block.Patterns) == 0 {
			continue
		}
		alias := block.Patterns[0].String()
		if alias == "" || strings.ContainsAny(alias, "*?!") {
			continue
		}
		h := sshConfigHost{Host: Host{Alias: alias, Port: 22, Tags: []string{}}}
		for _, node := range block.Nodes {
			kv, ok := node.(*ssh_config.KV)
			if !ok {
				continue
			}
			v := strings.TrimSpace(kv.Value)
			switch strings.ToLower(kv.Key) {
			case "hostname":
				h.Hostname = v
			case "user":
				h.User = v
			case "port":
				if p, err := strconv.Atoi(v); err == nil {
					h.Port = p
				}
			case "identityfile":
				h.IdentityFile = v
			case "proxyjump":
				h.ProxyJump = v
			}
		}
		if h.Hostname == "" {
			h.Hostname = alias
		}
		out = append(out, h)
	}
	return out, nil
}

// jumpAlias reduces a ProxyJump hop ("user@alias:port") to its alias.
func jumpAlias(hop string) string {
	hop = strings.TrimSpace(hop)
	if i := strings.LastIndex(hop, "@"); i >= 0 {
		hop = hop[i+1:]
	}
	if i := strings.LastIndex(hop, ":"); i >= 0 && !strings.Contains(hop[:i], ":") {
		hop = hop[:i]
	}
	return hop
}

// ImportSSHConfig adds the hosts in an ssh_config whose alias is not saved
// yet, and links each ProxyJump hop by alias to an imported or saved host
// (R32). A host whose jump alias matches no host is imported without a jump
// host and named in notes.
func (d *Data) ImportSSHConfig(r io.Reader) (imported int, notes []string, err error) {
	parsed, err := parseSSHConfig(r)
	if err != nil {
		return 0, nil, err
	}
	byAlias := map[string]string{}
	for _, h := range d.Hosts.Items {
		if _, ok := byAlias[h.Alias]; !ok {
			byAlias[h.Alias] = h.ID
		}
	}
	type pending struct {
		id   string
		jump string
	}
	var added []pending
	fresh := map[string]bool{}
	for _, p := range parsed {
		if _, ok := byAlias[p.Alias]; ok {
			continue
		}
		h := d.AddHost(p.Host)
		byAlias[h.Alias] = h.ID
		fresh[h.ID] = true
		added = append(added, pending{h.ID, p.ProxyJump})
		imported++
	}
	for _, p := range added {
		if p.jump == "" || strings.EqualFold(p.jump, "none") {
			continue
		}
		var hops []string
		for _, hop := range strings.Split(p.jump, ",") {
			id, ok := byAlias[jumpAlias(hop)]
			if !ok {
				notes = append(notes, fmt.Sprintf("%s: jump host %q is not a saved host, so it was imported without one", d.Host(p.id).Alias, jumpAlias(hop)))
				hops = nil
				break
			}
			hops = append(hops, id)
		}
		if len(hops) == 0 {
			continue
		}
		d.Host(p.id).JumpHostID = &hops[len(hops)-1]
		// "ProxyJump a,b": b is reached through a. Only set that on a hop this
		// import created and that names no jump of its own.
		for i := 1; i < len(hops); i++ {
			if hop := d.Host(hops[i]); fresh[hop.ID] && hop.JumpHostID == nil {
				hop.JumpHostID = &hops[i-1]
			}
		}
	}
	return imported, notes, nil
}
