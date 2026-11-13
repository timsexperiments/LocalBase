package upgrade

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// CheckReplaceable refuses a binary `upgrade` cannot replace: one Homebrew installed, one in a
// directory it can't write, or one with no release for this platform.
func CheckReplaceable(binary string) error {
	if strings.Contains(binary, "/Cellar/") {
		return errors.New("This was installed by Homebrew. Update it with: brew upgrade local-base")
	}
	dir := filepath.Dir(binary)
	probe, err := os.CreateTemp(dir, ".local-base-upgrade-probe-")
	if err != nil {
		return fmt.Errorf("Cannot write in %s, so this cannot replace the binary there. Nothing was changed.", dir)
	}
	probe.Close()
	os.Remove(probe.Name())
	if AssetArchive(goruntime.GOOS, goruntime.GOARCH) == "" {
		return fmt.Errorf("There are no releases for %s/%s.", goruntime.GOOS, goruntime.GOARCH)
	}
	return nil
}
