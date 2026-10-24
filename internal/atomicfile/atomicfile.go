// Package atomicfile replaces a file whole, so a concurrent reader sees the
// old bytes or the new ones and never a truncated file.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write replaces path with data: written to a temporary file beside it (named
// by pattern, as os.CreateTemp names one, and created 0600 — the headless
// token file depends on that mode), synced, and renamed over path. Synced as
// well as renamed: these files hold human decisions and credentials, and a
// crash between the two must not leave the rename pointing at nothing.
func Write(path, pattern string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename lands
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
