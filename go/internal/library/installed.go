package library

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// Installed lists the root's complete catalog installs plus loose model files, of kind or of each of
// kinds when kind is "" (then as `kind:name`), sorted.
func Installed(r *root.Root, cat *catalog.Catalog, kinds []string, kind string) []string {
	found := []string{}
	for _, k := range kinds {
		if kind != "" && k != kind {
			continue
		}
		dir := r.ModelsDir(k)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		known := map[string]bool{}
		var names []string
		for _, s := range cat.All() {
			if s.Kind != k {
				continue
			}
			for _, a := range s.Artifacts {
				known[a.Filename] = true
			}
			if complete, _ := s.Installation(dir); complete {
				names = append(names, s.ModelID)
			}
		}
		for _, en := range entries {
			if LooseModelFile(dir, en.Name()) && !known[en.Name()] {
				names = append(names, en.Name())
			}
		}
		for _, n := range names {
			if kind == "" {
				n = k + ":" + n
			}
			found = append(found, n)
		}
	}
	sort.Strings(found)
	return found
}

// LooseModelFile reports whether a models directory entry is a model file
// someone placed there: a regular file (or a link to one) with a model
// extension. Dot-prefixed entries are staging (benchmark parts, partial
// downloads, checksum caches) and directories are never models, even when
// their name ends in .gguf.
func LooseModelFile(dir, name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	if !slices.Contains([]string{".gguf", ".bin", ".onnx", ".safetensors", ".pth"}, filepath.Ext(name)) {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && info.Mode().IsRegular()
}
