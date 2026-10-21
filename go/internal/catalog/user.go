package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// DecodeSpec validates a catalog entry like a built-in one.
func DecodeSpec(b []byte) (*Spec, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return decodeStrict(m)
}

var safeFilenameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// UserEntryPath is <root>/catalog.d/<modelId>.json for a safe id.
func UserEntryPath(rootDir, modelID string) (string, error) {
	name := modelID + ".json"
	if !safeFilenameRe.MatchString(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("%q is not a safe catalog file name", name)
	}
	return filepath.Join(rootDir, "catalog.d", name), nil
}

// WriteUserEntry writes a complete user entry atomically, as JSON with
// two-space indentation (TS writeUserCatalogEntry).
func WriteUserEntry(rootDir string, spec *jsjson.Object) (string, *Spec, error) {
	raw := jsjson.Stringify(spec)
	s, err := DecodeSpec(raw)
	if err != nil {
		return "", nil, err
	}
	path, err := UserEntryPath(rootDir, s.ModelID)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", nil, err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, append(jsjson.Indent(spec), '\n'), 0o600); err != nil {
		return "", nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", nil, err
	}
	return path, s, nil
}

// RemoveUserEntry deletes the user file that provides modelID ("" when no
// user file does).
func RemoveUserEntry(rootDir string, cat *Catalog, modelID string) (string, error) {
	dir := filepath.Join(rootDir, "catalog.d")
	path := ""
	if s := cat.ByID(modelID); s != nil && s.Tier == "user" && strings.HasPrefix(s.LayerPath, dir+string(filepath.Separator)) {
		path = s.LayerPath
	}
	if path == "" {
		// A skipped (invalid) file named after the model can still be removed.
		if candidate, err := UserEntryPath(rootDir, modelID); err == nil {
			if info, err := os.Lstat(candidate); err == nil && info.Mode().IsRegular() {
				path = candidate
			}
		}
	}
	if path == "" {
		return "", nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return path, nil
}

// AnnotationsForRepo are every annotation for a repository, any revision.
func (c *Catalog) AnnotationsForRepo(repo string) []Annotation {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Annotation
	for _, i := range c.byRepo[strings.ToLower(repo)] {
		out = append(out, c.Annotations[i])
	}
	return out
}

// RecipeAnnotation is the published annotation whose recipe installs modelID.
func (c *Catalog) RecipeAnnotation(modelID string) *Annotation {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i := range c.Annotations {
		if r := c.Annotations[i].Recipe; r != nil && r.ModelID == modelID {
			return &c.Annotations[i]
		}
	}
	return nil
}
