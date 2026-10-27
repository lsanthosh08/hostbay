package store

// Script is a saved command button.
type Script struct {
	Meta
	Label  string `json:"label"`
	Script string `json:"script"`
}

// defaultScripts are shown until the user first saves scripts. sudo -n so
// they run without a terminal to prompt on.
var defaultScripts = [][2]string{
	{"nginx -t", "sudo -n nginx -t 2>&1"},
	{"Reload nginx", "sudo -n nginx -t && sudo -n systemctl reload nginx 2>&1"},
	{"Update Package List", "sudo -n apt-get update"},
	{"Install Nginx", "sudo -n apt-get update && sudo -n apt-get install -y nginx"},
	{"Install Docker", "curl -fsSL https://get.docker.com | sh"},
	{"Docker clean", "docker system prune -af"},
	{"Reboot", "sudo -n reboot"},
}

// ScriptList returns the saved scripts, or the defaults (with fresh IDs)
// when none were ever saved.
func (d *Data) ScriptList() []Script {
	if d.Scripts.Items != nil {
		return d.Scripts.Items
	}
	out := []Script{}
	for _, s := range defaultScripts {
		out = append(out, Script{Meta: Meta{ID: NewID()}, Label: s[0], Script: s[1]})
	}
	return out
}

// AddScript adds s, keeping any Meta it carries.
func (d *Data) AddScript(s Script) Script { return add(d, &d.Scripts, s) }

// SetScripts makes in the saved list, in its order. Only scripts that are
// new or changed are touched, and dropped ones leave tombstones.
func (d *Data) SetScripts(in []Script) {
	old := d.Scripts.Items
	d.Scripts.Items = []Script{}
	for _, s := range in {
		i := find(old, s.ID)
		if i < 0 {
			s.Meta = Meta{ID: s.ID}
			d.AddScript(s)
			continue
		}
		cur := old[i]
		if cur.Label != s.Label || cur.Script != s.Script {
			cur.Label, cur.Script = s.Label, s.Script
			d.st.Touch(&cur.Meta)
		}
		d.Scripts.Items = append(d.Scripts.Items, cur)
		old = append(old[:i], old[i+1:]...)
	}
	for _, s := range old {
		d.Scripts.Items = append(d.Scripts.Items, s) // purge needs it present
		purge(d, &d.Scripts, s.ID)
	}
}
