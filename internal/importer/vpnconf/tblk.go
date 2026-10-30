package vpnconf

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"hostbay/internal/importer"
)

// tblk imports a Tunnelblick bundle: its OpenVPN configuration, at the top
// or in Contents/Resources, with the certificate files beside it.
func tblk(b *importer.Builder, dir, name string) error {
	for _, sub := range []string{dir, filepath.Join(dir, "Contents", "Resources")} {
		entries, _ := os.ReadDir(sub)
		for _, e := range entries {
			if ext := strings.ToLower(filepath.Ext(e.Name())); !e.IsDir() && slices.Contains([]string{".ovpn", ".conf"}, ext) {
				raw, err := importer.ReadFile(filepath.Join(sub, e.Name()))
				if err != nil {
					return err
				}
				ovpn(b, name, importer.Text(raw), sub)
				return nil
			}
		}
	}
	return fmt.Errorf("%s holds no OpenVPN configuration", filepath.Base(dir))
}
