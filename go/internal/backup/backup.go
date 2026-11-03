// Package backup writes a root's settings to one private file, and restores them (DESIGN §34).
//
// A backup holds secrets, and restoring one decides who can sign in and which programs run as
// you. So the file is private, nothing here reaches a network, and restore treats everything in
// a file as input from someone else, its manifest included. TestNoNetworkImports reads this
// package's imports.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const (
	// Format is what this version writes and reads.
	Format       = "localbase.backup/v1"
	formatPrefix = "localbase.backup/v"
	// Extension is a backup's file extension.
	Extension     = ".lbbackup"
	manifestEntry = "manifest.json"
	filesPrefix   = "files/"
	beforePrefix  = ".before-restore-"

	// SignOutLine and RevokedLine are said before a restore asks, and are in every manifest.
	SignOutLine = "A restore signs everyone out. API keys keep working."
	RevokedLine = "A key revoked, or a user removed or disabled, on the root being replaced stays that way, whatever this backup says."
)

// These limits are constants, because a backup is input from someone else: none of them is ever
// taken from a manifest.
const (
	maxManifestBytes = 8 << 20
	maxFileBytes     = 8 << 30
	maxTotalBytes    = 16 << 30
	maxEntries       = 20000
	maxPathLength    = 512
)

// Manifest is the first entry of a backup. Restore checks every file against it and believes
// nothing else it says.
type Manifest struct {
	Format         string          `json:"format"`
	CreatedAt      string          `json:"createdAt"`
	Version        string          `json:"version"`
	GoSchema       int             `json:"goSchema"`
	Migrations     int             `json:"migrations"`
	Secrets        string          `json:"secrets"` // included | excluded
	SecretNames    []string        `json:"secretNames"`
	SecretsRemoved []RemovedSecret `json:"secretsRemoved"`
	Files          []File          `json:"files"`
	NotIncluded    []NotIncluded   `json:"notIncluded"`
	RowsLeftOut    []string        `json:"rowsLeftOut"`
	Models         []string        `json:"modelsToInstall"`
	RestoreRules   []string        `json:"restoreRules"`
}

// File is one file in a backup, by its path in a root.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// NotIncluded is something a root holds that a backup leaves out, and why.
type NotIncluded struct {
	What    string `json:"what"`
	Why     string `json:"why"`
	Bytes   int64  `json:"bytes"`
	Unknown bool   `json:"unknown,omitempty"`
}

// includedFiles and includedDirs are all a backup holds, and all a restore accepts.
var (
	includedFiles = []string{"local-base.db", "ui-access.json", "email-delivery.json", "credentials/auth-profiles.json"}
	includedDirs  = []string{"catalog.d", "backends.d", "voices"}
	// notIncludedTop says why each other thing a root holds stays out.
	notIncludedTop = map[string]string{
		"models":                "Large, and downloaded again. Restore lists which to install",
		"runs":                  "Replies in progress. No prompt or reply ever goes in a backup",
		"logs":                  "Not settings, and they stay on the computer they describe",
		"bin":                   "Downloaded again, and programs should not travel in a data file",
		"runtime":               "Tied to this machine and this process",
		"service":               "Tied to this machine. `start` writes it again",
		"cache":                 "Fetched again",
		"catalog-feed.json":     "A cache of the remote catalog, fetched again",
		".localbase-root.json":  "Restore makes its own, since a marker names one root",
		"local-base.db-wal":     "Part of the database, which is copied whole",
		"local-base.db-shm":     "Part of the database, which is copied whole",
		root.RestoreJournalName: "Left by a restore that did not finish",
	}
	// rowsLeftOut are rows that are not settings. They are dropped when a backup is written and
	// again on restore, since a crafted file can carry them.
	rowsLeftOut = []string{"auth_browser_sessions", "local_sessions", "local_login_links", "auth_magic_link_tokens",
		"model_observed_memory", "model_runtime_capabilities", "config_activation"}
)

// Options says what to back up, and where.
type Options struct {
	Root           *root.Root
	Output         string // "" is the default place
	ExcludeSecrets bool
	DryRun         bool
	Version        string
	Now            time.Time
}

// Result says what happened. Warning always goes to the person.
type Result struct {
	Output   string    `json:"output,omitempty"`
	Bytes    int64     `json:"bytes,omitempty"`
	DryRun   bool      `json:"dryRun,omitempty"`
	Warning  string    `json:"warning"`
	Cautions []string  `json:"cautions,omitempty"`
	Manifest *Manifest `json:"manifest"`
}

// DefaultDir is where backups go unless told otherwise: beside the root and not in it, so
// uninstall leaves them, and a backup never holds a backup.
func DefaultDir(rootDir string) string { return rootDir + "-backups" }

// Warning is said every time a backup is made.
func Warning(m *Manifest) string {
	if len(m.SecretNames) > 0 {
		return "This file contains your secrets and is not encrypted: " + strings.Join(m.SecretNames, ", ") + ".\n" +
			"Anyone who can read it can use them. Keep it like a password; LocalBase never uploads it."
	}
	return "This file leaves out your secrets and is not encrypted. It is not anonymised: it still holds\n" +
		"users' email addresses, API key hashes, voice recordings and usage totals."
}

// survey sorts every entry of a root into what a backup holds and what it leaves out.
func survey(rootDir string) (files []string, left []NotIncluded, err error) {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, nil, err
	}
	sizeOf := func(p string) int64 {
		var total int64
		filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if info, err := d.Info(); err == nil {
					total += info.Size()
				}
			}
			return nil
		})
		return total
	}
	unknown := func(rel string, p string) {
		left = append(left, NotIncluded{What: rel, Why: "Not something this version knows, so it is not copied", Bytes: sizeOf(p), Unknown: true})
	}
	for _, e := range entries {
		name, p := e.Name(), filepath.Join(rootDir, e.Name())
		switch {
		case name == "credentials":
			inner, err := os.ReadDir(p)
			if err != nil {
				return nil, nil, err
			}
			for _, c := range inner {
				rel := "credentials/" + c.Name()
				if rel == "credentials/auth-profiles.json" {
					files = append(files, rel)
				} else {
					unknown(rel, filepath.Join(p, c.Name()))
				}
			}
		case contains(includedFiles, name):
			files = append(files, name)
		case contains(includedDirs, name):
			inner, err := os.ReadDir(p)
			if err != nil {
				return nil, nil, err
			}
			for _, c := range inner {
				rel := name + "/" + c.Name()
				switch {
				case strings.HasSuffix(c.Name(), ".tmp"):
					left = append(left, NotIncluded{What: rel, Why: "A temporary file", Bytes: sizeOf(filepath.Join(p, c.Name()))})
				case c.IsDir():
					unknown(rel, filepath.Join(p, c.Name()))
				default:
					// A link stays on this list, so that reading it refuses, and says so.
					files = append(files, rel)
				}
			}
		case strings.HasPrefix(name, beforePrefix):
			left = append(left, NotIncluded{What: name, Why: "Old files from an earlier restore. They hold secrets, and a backup must not carry them on", Bytes: sizeOf(p)})
		default:
			why, known := notIncludedTop[name]
			if !known {
				unknown(name, p)
				continue
			}
			if e.IsDir() {
				name += "/"
			}
			left = append(left, NotIncluded{What: name, Why: why, Bytes: sizeOf(p)})
		}
	}
	sort.Strings(files)
	sort.Slice(left, func(i, j int) bool { return left[i].What < left[j].What })
	return files, left, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// destination says where a backup goes, and refuses what would make it less than private.
func destination(o Options) (string, []string, error) {
	name := "local-base-" + o.Now.UTC().Format("20060102-150405") + Extension
	if o.Output == "" {
		dir := DefaultDir(o.Root.Dir)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if !o.DryRun {
				if err := os.Mkdir(dir, 0o700); err != nil {
					return "", nil, err
				}
				if err := os.Chmod(dir, 0o700); err != nil {
					return "", nil, err
				}
			}
		case err != nil:
			return "", nil, err
		case !info.IsDir():
			return "", nil, fmt.Errorf("%s is not a directory", dir)
		case !ownedByMe(info):
			return "", nil, fmt.Errorf("another user owns %s", dir)
		case info.Mode().Perm()&0o077 != 0:
			return "", nil, fmt.Errorf("other users can reach %s, and a backup holds secrets. Run: chmod 700 '%s', or choose a place with --output", dir, dir)
		}
		return filepath.Join(dir, name), nil, nil
	}
	out, err := filepath.Abs(o.Output)
	if err != nil {
		return "", nil, err
	}
	if !strings.HasSuffix(out, Extension) && isDir(out) {
		out = filepath.Join(out, name)
	}
	if _, err := os.Lstat(out); err == nil {
		return "", nil, fmt.Errorf("%s already exists, and a backup never overwrites a file", out)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", nil, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(out))
	if err != nil {
		return "", nil, err
	}
	out = filepath.Join(dir, filepath.Base(out))
	if inside(out, o.Root.Dir) {
		return "", nil, fmt.Errorf("%s is inside the root, where the next backup and uninstall would reach it", out)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", nil, err
	}
	if !ownedByMe(info) && info.Mode().Perm()&0o022 != 0 {
		return "", nil, fmt.Errorf("other users can change %s, and a backup holds secrets", dir)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", nil, fmt.Errorf("other users can change %s, and a backup holds secrets. Choose another place, or run: chmod go-w '%s'", dir, dir)
	}
	var cautions []string
	if info.Mode().Perm()&0o055 != 0 {
		cautions = append(cautions, "Other users can list "+dir+". The file itself is private to you.")
	}
	return out, cautions, nil
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func inside(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Create writes a backup, or with DryRun says what it would hold.
func Create(ctx context.Context, o Options) (*Result, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	dbPath := o.Root.DB()
	if _, err := os.Lstat(dbPath); err != nil {
		return nil, fmt.Errorf("%s has no database to back up: %w", o.Root.Dir, err)
	}
	files, left, err := survey(o.Root.Dir)
	if err != nil {
		return nil, err
	}
	secrets, err := readSecrets(ctx, o.Root.Dir, o.ExcludeSecrets)
	if err != nil {
		return nil, err
	}
	migrations, err := appliedMigrations(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Format: Format, CreatedAt: o.Now.UTC().Format(time.RFC3339), Version: o.Version, GoSchema: store.GoSchemaVersion(),
		Migrations: migrations, Secrets: "included", SecretNames: secrets.names, SecretsRemoved: secrets.removed, NotIncluded: left,
		RowsLeftOut: presentRows(ctx, dbPath), Models: selectedModels(ctx, dbPath), RestoreRules: []string{SignOutLine, RevokedLine}}
	if o.ExcludeSecrets {
		m.Secrets = "excluded"
	}
	for _, rel := range files {
		if _, dropped := secrets.dropped[rel]; dropped {
			m.NotIncluded = append(m.NotIncluded, NotIncluded{What: rel, Why: "It holds a secret, and --exclude-secrets was given"})
		}
	}
	out, cautions, err := destination(o)
	if err != nil {
		return nil, err
	}
	res := &Result{Output: out, DryRun: o.DryRun, Warning: Warning(m), Cautions: cautions, Manifest: m}
	if o.DryRun {
		for _, rel := range files {
			if _, dropped := secrets.dropped[rel]; dropped {
				continue
			}
			info, err := os.Lstat(filepath.Join(o.Root.Dir, filepath.FromSlash(rel)))
			if err != nil {
				return nil, err
			}
			m.Files = append(m.Files, File{Path: rel, Size: info.Size()})
		}
		return res, nil
	}

	stage, err := os.MkdirTemp(filepath.Dir(out), ".lbbackup-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o700); err != nil {
		return nil, err
	}
	content := filepath.Join(stage, "content")
	if err := os.Mkdir(content, 0o700); err != nil {
		return nil, err
	}
	var total int64
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, dropped := secrets.dropped[rel]; dropped {
			continue
		}
		to := filepath.Join(content, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return nil, err
		}
		if rel == "local-base.db" {
			if err := copyDatabase(ctx, dbPath, to, stage, o.ExcludeSecrets); err != nil {
				return nil, err
			}
		} else {
			body, reduced := secrets.reduced[rel]
			if !reduced {
				if body, err = obs.ReadOwnedRegular(filepath.Join(o.Root.Dir, filepath.FromSlash(rel))); err != nil {
					return nil, err
				}
			}
			if err := writePrivate(to, body); err != nil {
				return nil, err
			}
		}
		f, err := describe(to, rel)
		if err != nil {
			return nil, err
		}
		if f.Size > maxFileBytes {
			return nil, fmt.Errorf("%s is %d bytes, more than a restore accepts", rel, f.Size)
		}
		if total += f.Size; total > maxTotalBytes {
			return nil, fmt.Errorf("the backup would hold more than a restore accepts (%d bytes)", int64(maxTotalBytes))
		}
		m.Files = append(m.Files, f)
	}
	if len(m.Files) > maxEntries {
		return nil, fmt.Errorf("the backup would hold %d files, more than a restore accepts", len(m.Files))
	}
	archive := filepath.Join(stage, "archive")
	if err := writeArchive(ctx, archive, content, m); err != nil {
		return nil, err
	}
	if err := place(archive, out); err != nil {
		return nil, err
	}
	info, err := os.Stat(out)
	if err != nil {
		return nil, err
	}
	res.Bytes = info.Size()
	return res, nil
}

// writePrivate writes a new file that is 0600 from its first moment, whatever the umask.
func writePrivate(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		return err
	}
	return f.Sync()
}

func describe(path, rel string) (File, error) {
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return File{}, err
	}
	return File{Path: rel, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func writeArchive(ctx context.Context, archive, content string, m *Manifest) error {
	out, err := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := out.Chmod(0o600); err != nil {
		return err
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if len(manifest) > maxManifestBytes {
		return errors.New("the manifest is larger than a restore accepts")
	}
	stamp, _ := time.Parse(time.RFC3339, m.CreatedAt)
	head := func(name string, size int64) *tar.Header {
		return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: size, ModTime: stamp, Format: tar.FormatPAX}
	}
	if err := tw.WriteHeader(head(manifestEntry, int64(len(manifest)))); err != nil {
		return err
	}
	if _, err := tw.Write(manifest); err != nil {
		return err
	}
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := tw.WriteHeader(head(path.Join(strings.TrimSuffix(filesPrefix, "/"), f.Path), f.Size)); err != nil {
			return err
		}
		in, err := os.Open(filepath.Join(content, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, in)
		in.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return out.Sync()
}

// place puts a finished backup at its name, and never over another file. A link fails if the
// name is taken; a filesystem without links gets an exclusive copy instead.
func place(archive, out string) error {
	err := os.Link(archive, out)
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s already exists, and a backup never overwrites a file", out)
	}
	in, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err == nil {
		if _, err = io.Copy(f, in); err == nil {
			err = f.Sync()
		}
		if err != nil {
			f.Close()
			os.Remove(out)
			return err
		}
	}
	return f.Close()
}
