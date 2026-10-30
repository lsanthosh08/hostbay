package vpnconf

import (
	"path/filepath"
	"strings"

	"hostbay/internal/importer"
)

// visc imports a Viscosity bundle: config.conf, an OpenVPN configuration
// whose "#viscosity name" comment names it, with the files beside it.
func visc(b *importer.Builder, dir string) error {
	raw, err := importer.ReadFile(filepath.Join(dir, "config.conf"))
	if err != nil {
		return err
	}
	text := importer.Text(raw)
	name := strings.TrimSuffix(filepath.Base(dir), filepath.Ext(dir))
	for _, line := range strings.Split(text, "\n") {
		if n, ok := strings.CutPrefix(strings.TrimSpace(line), "#viscosity name "); ok {
			name = strings.TrimSpace(n)
		}
	}
	ovpn(b, name, text, dir)
	return nil
}
