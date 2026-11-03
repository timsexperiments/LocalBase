package root

import (
	"io/fs"
	"os"
	"path/filepath"
)

// TightenPermissions makes a root private: directories 0700, and no group or
// other access on any file, an owner's execute bit kept. It leaves models/ and
// bin/ alone: they hold no secrets and can be large, and bin/ keeps its packages'
// modes. It never follows a symlink, does nothing to what is already tight, and
// never fails: fail hears about anything it could not change. It reports how many
// entries it changed.
func TightenPermissions(dir string, fail func(path string, err error)) int {
	changed := 0
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			fail(path, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() && path != dir && (d.Name() == "models" || d.Name() == "bin") && filepath.Dir(path) == dir {
			return fs.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			fail(path, err)
			return nil
		}
		mode := info.Mode().Perm()
		want := mode &^ 0o077
		if d.IsDir() {
			want = 0o700
		}
		if mode == want {
			return nil
		}
		if err := os.Chmod(path, want); err != nil {
			fail(path, err)
			return nil
		}
		changed++
		return nil
	})
	if walkErr != nil {
		fail(dir, walkErr)
	}
	return changed
}
