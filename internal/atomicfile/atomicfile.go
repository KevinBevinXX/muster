// Package atomicfile writes a whole file so that no reader ever sees half of
// it: a temp file unique to this write, in the same directory, 0600 from
// creation, synced, then renamed into place. Concurrent writers never
// interleave — the file is always one whole write, the last one's.
//
// The ONE such writer muster-mirror has: the hook config (`muster-mirror
// ask`'s record), the Hermes status files and the Hermes allowlist all go
// through it.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write replaces path with b, mode 0600.
func Write(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp") // 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o600)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}
