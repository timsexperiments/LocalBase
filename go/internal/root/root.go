// Package root locates a LocalBase data directory and the paths inside it.
//
// The layout is shared with the TypeScript implementation, so a root created
// by either binary can be served by the other.
package root

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/lease"
)

const markerName = ".localbase-root.json"

// Root is a validated, canonical LocalBase data directory.
type Root struct {
	Dir   string
	owner *Owner
}

type marker struct {
	Version  int    `json:"version"`
	Root     string `json:"root"`
	RootHash string `json:"rootHash"`
	Identity string `json:"identity"`
}

// Default is ~/.local/share/local-base, the same default as the TS binary.
func Default() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "local-base")
}

// IsDefault says whether dir is the default root, however its path is spelled. What LOCALBASE_ROOT names
// isn't the default, even when a command would use it.
func IsDefault(dir string) bool {
	def := Default()
	if def == "" {
		return false
	}
	// A path that can't be made canonical is compared as written, so a guard on the default root
	// doesn't let a spelling it can't resolve through.
	a, err := Canonical(dir)
	if err != nil {
		a = dir
	}
	b, err := Canonical(def)
	if err != nil {
		b = def
	}
	return a == b
}

// DefaultRootNote is what a message about stopping a gateway adds when the root is the default one, so
// that the person reading it knows which LocalBase it means. It names the root and not the service,
// because a gateway on that root may have been started with serve.
func DefaultRootNote(dir string) string {
	if IsDefault(dir) {
		return " This is the default root, the one your installed LocalBase uses."
	}
	return ""
}

// Resolve picks the root from an explicit flag, then LOCALBASE_ROOT, then the default.
func Resolve(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if env := os.Getenv("LOCALBASE_ROOT"); env != "" {
		return env
	}
	return Default()
}

// Hash is the sha256 of the canonical path; it names the service unit. It is lease.Hash, which also names
// the root's operation lease.
func Hash(dir string) string { return lease.Hash(dir) }

func canonical(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("root %q must be an absolute path", dir)
	}
	if strings.ContainsAny(dir, "\x00\r\n") {
		return "", errors.New("root must not contain NUL or line breaks")
	}
	return resolveExistingAncestor(filepath.Clean(dir))
}

// resolveExistingAncestor resolves symlinks in the deepest existing ancestor
// and keeps the missing suffix, like the TS canonicalLocalBaseRoot (on macOS
// /tmp is /private/tmp, and the marker records the resolved path).
func resolveExistingAncestor(p string) (string, error) {
	suffix := []string{}
	probe := p
	for {
		if real, err := filepath.EvalSymlinks(probe); err == nil {
			return filepath.Join(append([]string{real}, suffix...)...), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return p, nil
		}
		suffix = append([]string{filepath.Base(probe)}, suffix...)
		probe = parent
	}
}

// Open validates an existing root's marker.
func Open(dir string) (*Root, error) { return openRoot(dir, options{}) }

func openRoot(dir string, o options) (*Root, error) {
	r, err := openForUndo(dir, o)
	if err != nil {
		return nil, err
	}
	if err := unfinishedRestore(r.Dir); err != nil {
		return nil, err
	}
	return r, nil
}

// OpenForUndo is Open for the one command that finishes an interrupted restore. Every
// other check still applies.
func OpenForUndo(dir string) (*Root, error) { return openForUndo(dir, options{}) }

func openForUndo(dir string, o options) (*Root, error) {
	dir, err := canonical(dir)
	if err != nil {
		return nil, err
	}
	if err := checkBeforeReading(dir, o); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, markerName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s is not an initialized LocalBase root; run `local-base init`", dir)
		}
		return nil, err
	}
	var m marker
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("invalid root marker in %s: %w", dir, err)
	}
	if m.Version != 1 || m.Root != dir || m.RootHash != Hash(dir) {
		return nil, fmt.Errorf("root marker in %s belongs to %q; refusing to use a copied or moved root", dir, m.Root)
	}
	if err := secure(dir, o); err != nil {
		return nil, err
	}
	return &Root{Dir: dir}, nil
}

// onlyLicenses: the v0.1.0 installer writes licenses/ into the default data directory,
// so a root holding only that real directory is one nobody has used yet.
func onlyLicenses(entries []os.DirEntry) bool {
	return len(entries) == 1 && entries[0].Name() == "licenses" && entries[0].IsDir()
}

// Init creates the directory and marker if absent and returns the root.
// A nonempty directory without a marker, / and the home directory are rejected.
func Init(dir string) (*Root, error) { return initRoot(dir, options{}) }

// initRoot is Init, with a test's options.
func initRoot(dir string, o options) (*Root, error) {
	dir, err := canonical(dir)
	if err != nil {
		return nil, err
	}
	if home, _ := os.UserHomeDir(); dir == "/" || (home != "" && dir == mustCanonical(home)) {
		return nil, fmt.Errorf("Refusing LocalBase operation for protected directory: %s.", dir)
	}
	r, err := openRoot(dir, o)
	if err == nil {
		return r, nil
	}
	var unsafe *UnsafeRootError
	if errors.As(err, &unsafe) {
		return nil, err
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 && !onlyLicenses(entries) {
		return nil, fmt.Errorf("Refusing to initialize nonempty LocalBase root: %s.", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The directory may have been there already, and open to others.
	if err := closeDirectory(dir, o); err != nil {
		return nil, err
	}
	m := marker{Version: 1, Root: dir, RootHash: Hash(dir), Identity: uuid()}
	raw, _ := json.Marshal(m)
	tmp := filepath.Join(dir, markerName+".tmp")
	if err := writeSynced(tmp, raw, o); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, markerName)); err != nil {
		return nil, err
	}
	return &Root{Dir: dir}, nil
}

func (r *Root) DB() string                   { return filepath.Join(r.Dir, "local-base.db") }
func (r *Root) CatalogDir() string           { return filepath.Join(r.Dir, "catalog.d") }
func (r *Root) LogsDir() string              { return filepath.Join(r.Dir, "logs") }
func (r *Root) VoicesDir() string            { return filepath.Join(r.Dir, "voices") }
func (r *Root) RuntimesDir() string          { return filepath.Join(r.Dir, "bin", "runtimes") }
func (r *Root) ModelsDir(kind string) string { return filepath.Join(r.Dir, "models", kind) }

func mustCanonical(p string) string {
	c, err := canonical(p)
	if err != nil {
		return p
	}
	return c
}

// Canonical is the canonical form of a root path (symlinks resolved through
// the deepest existing ancestor).
func Canonical(dir string) (string, error) { return canonical(dir) }
