// Package defaults carries the starter pre and post scripts that are copied
// to the data volume on first start.
package defaults

import (
	"embed"
	"os"
	"path/filepath"
)

//go:embed pre.sh post.sh
var files embed.FS

// Install writes pre.sh and post.sh into dir unless they already exist, so
// edits made on the volume are never overwritten.
func Install(dir string) error {
	for _, name := range []string{"pre.sh", "post.sh"} {
		dst := filepath.Join(dir, name)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		data, err := files.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return err
		}
	}
	return nil
}
