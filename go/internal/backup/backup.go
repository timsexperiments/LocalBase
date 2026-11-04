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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
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
	// temporaryPrefix names a working directory, so one a killed run leaves is obvious. It holds
	// what a backup holds, secrets included, for as long as it exists.
	temporaryPrefix = ".local-base-temporary-"
	// formerLog is the one name in those directories that isn't a JSON entry: the ids of former
	// built-ins already kept as entries, so that removing one sticks.
	formerLog = "catalog.d/.former-built-ins"

	// SignOutLine, HeldKeyLine, RevokedLine and ChangedKeyLine are said before a restore asks, and are in every manifest.
	SignOutLine = "A restore signs everyone out. Active API keys keep working, except as said here, and revoked ones stay revoked."
	RevokedLine = "A key revoked, or a user removed or disabled, on the root being replaced stays that way, whatever this backup says."
	// HeldKeyLine is for where a file could bring in an administrator's key unnoticed: nothing on a
	// root with no keys would show it was never there.
	HeldKeyLine = "On a root with no keys, a key that can do more than make requests stays out, unless you name it with --bring-back-key."
	// ChangedKeyLine covers what RevokedLine doesn't: a key rotated because its secret got out.
	ChangedKeyLine = "On a root with keys of its own, a key changed there keeps its current secret and permissions, and a key missing there stays out, unless you name it with --bring-back-key."
)

// These limits are constants, because a backup is input from someone else: none of them is ever
// taken from a manifest.
const (
	maxManifestBytes = 8 << 20
	maxFileBytes     = 8 << 30
	maxTotalBytes    = 16 << 30
	maxEntries       = 20000
	// What a backup can really hold. A compressed file can list far more than it weighs, and these
	// files are read into memory, so each is held to what a root writes. The catalog feed has the
	// same limit, and a voice can't be larger than its upload.
	maxSettingsFileBytes  = 8 << 20
	maxSettingsTotalBytes = 64 << 20
	maxFormerBytes        = 1 << 20
	// What a database can really hold. Usage piles up, and nothing here reads it. Everything else is
	// settings, read into memory, and small.
	maxDatabaseRows       = 100_000
	maxDatabaseTableBytes = 16 << 20
	maxDatabaseValueBytes = 1 << 20
	maxPathLength         = 512
)

// Manifest is the first entry of a backup. Restore checks every file against it and believes
// nothing else it says.
type Manifest struct {
	Format         string            `json:"format"`
	CreatedAt      string            `json:"createdAt"`
	Version        string            `json:"version"`
	GoSchema       int               `json:"goSchema"`
	Migrations     int               `json:"migrations"`
	Secrets        string            `json:"secrets"` // included | excluded
	SecretNames    []string          `json:"secretNames"`
	SecretsRemoved []RemovedSecret   `json:"secretsRemoved"`
	Files          []File            `json:"files"`
	NotIncluded    []NotIncluded     `json:"notIncluded"`
	RowsLeftOut    []string          `json:"rowsLeftOut"`
	Models         []string          `json:"modelsToInstall"`
	Issuers        []IssuerLine      `json:"issuers"`
	Unbound        auth.UnboundCount `json:"unbound"`
	RestoreRules   []string          `json:"restoreRules"`
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
		"local-base.db-journal": "SQLite's record of a write in progress. The copy holds only finished writes",
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
		// An estimate for the report: what can't be read counts as nothing, and the walk never stops.
		_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
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
	ignored, err := ignoredByTheGateway(rootDir)
	if err != nil {
		return nil, nil, err
	}
	// listed sorts one file of a directory a backup holds from: the same rule restore uses.
	listed := func(rel, p string, entry fs.DirEntry) {
		switch {
		case strings.HasSuffix(entry.Name(), ".tmp"):
			left = append(left, NotIncluded{What: rel, Why: "A temporary file", Bytes: sizeOf(p)})
		case !holdable(rel):
			unknown(rel, p)
		case entry.Type()&fs.ModeSymlink != 0:
			// It stays on the list, so that reading it refuses, and says so.
			files = append(files, rel)
		case ignored[rel] != "":
			left = append(left, NotIncluded{What: rel, Why: "LocalBase ignores it, and a restore would refuse it: " + ignored[rel], Bytes: sizeOf(p)})
		default:
			files = append(files, rel)
		}
	}
	for _, e := range entries {
		name, p := e.Name(), filepath.Join(rootDir, e.Name())
		switch {
		case name == "credentials" || contains(includedDirs, name):
			inner, err := os.ReadDir(p)
			if err != nil {
				return nil, nil, err
			}
			for _, c := range inner {
				listed(name+"/"+c.Name(), filepath.Join(p, c.Name()), c)
			}
		case contains(includedFiles, name):
			files = append(files, name)
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

var temporaryRe = regexp.MustCompile(`^\.local-base-temporary-(backup|restore)-([0-9]+)-[0-9]+$`)

// newTemporary makes a private working directory, named for what made it and its process.
func newTemporary(dir, kind string) (string, error) {
	path, err := os.MkdirTemp(dir, fmt.Sprintf("%s%s-%d-", temporaryPrefix, kind, os.Getpid()))
	if err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit; 0700 is owner only.
		os.RemoveAll(path)
		return "", err
	}
	return path, nil
}

// abandoned lists working directories in dir whose process is gone. A process that is killed can't
// clean up, so these are found afterwards, by whoever looks next.
func abandoned(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		m := temporaryRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		info, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil || !info.IsDir() || !ownedByMe(info) {
			continue
		}
		pid, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
			continue // that process is still working
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

// Abandoned lists what killed runs left around a root, for doctor. It removes nothing.
func Abandoned(rootDir string) []string {
	return append(abandoned(filepath.Dir(rootDir)), abandoned(DefaultDir(rootDir))...)
}

// sweep removes what killed runs left in these places, and says so.
func sweep(places ...string) []string {
	var said []string
	seen := map[string]bool{}
	for _, place := range places {
		if seen[place] {
			continue
		}
		seen[place] = true
		for _, path := range abandoned(place) {
			if err := os.RemoveAll(path); err != nil {
				said = append(said, "An interrupted run left private files at "+path+", and they could not be removed. They may hold secrets: "+err.Error())
				continue
			}
			said = append(said, "Removed private files an interrupted run left at "+path+".")
		}
	}
	return said
}

// ctxReader lets a long copy stop when the run is interrupted.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// holdable is every path a backup may hold, for writing one and for reading one. They must not
// disagree: a file one writes and the other refuses makes a backup that can never be restored.
// Each directory holds only what its reader reads. Anything else there is listed as unknown.
// maxBytesOf is the most a file in a backup can be, by what it is. A file holdable says nothing about
// is a setting, so it can't be read for more than one.
func maxBytesOf(rel string) int64 {
	switch {
	case rel == "local-base.db":
		return maxFileBytes
	case rel == formerLog:
		return maxFormerBytes
	case strings.HasPrefix(rel, "voices/"):
		return voices.MaxUploadBytes
	}
	return maxSettingsFileBytes
}

// isSettingsFile says whether a file counts toward what a restore reads into memory.
func isSettingsFile(rel string) bool {
	return rel != "local-base.db" && !strings.HasPrefix(rel, "voices/")
}

func holdable(rel string) bool {
	if len(rel) == 0 || len(rel) > maxPathLength || strings.ContainsAny(rel, "\\\x00\r\n") || path.IsAbs(rel) || path.Clean(rel) != rel {
		return false
	}
	if contains(includedFiles, rel) || rel == formerLog {
		return true
	}
	dir, name := path.Split(rel)
	switch strings.TrimSuffix(dir, "/") {
	case "catalog.d", "backends.d":
		return strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".")
	case "voices":
		id, wav := strings.CutSuffix(name, ".wav")
		return wav && voices.IDPattern.MatchString(id)
	}
	return false
}

// backendProblem is why a custom backend's file is not usable, or nil. The gateway skips such a
// file, and a restore refuses it.
func backendProblem(file string, builtin *backends.Registry) (*backends.Definition, error) {
	d, err := backends.ReadCustom(file)
	if err != nil {
		return nil, err
	}
	if filepath.Base(file) != d.ID+".json" {
		return nil, fmt.Errorf("it should be named %s.json", d.ID)
	}
	if builtin.Get(d.ID) != nil {
		return nil, fmt.Errorf("it has the id of a built-in backend, %s", d.ID)
	}
	return d, nil
}

// ignoredByTheGateway is what the gateway reads past, by path, with why. A real root can hold a
// broken entry for years, since the gateway only reports it. A backup leaves it out and says so.
func ignoredByTheGateway(rootDir string) (map[string]string, error) {
	out := map[string]string{}
	cat, err := catalog.Load(filepath.Join(rootDir, "catalog.d"))
	if err != nil {
		return nil, err
	}
	for _, d := range cat.Diagnostics {
		if d.Tier == "user" && d.Path != "" {
			out["catalog.d/"+filepath.Base(d.Path)] = d.Message
		}
	}
	builtin, err := backends.Load("")
	if err != nil {
		return nil, err
	}
	files, _ := filepath.Glob(filepath.Join(backends.Dir(rootDir), "*.json"))
	for _, file := range files {
		if _, err := backendProblem(file, builtin); err != nil {
			out["backends.d/"+filepath.Base(file)] = err.Error()
		}
	}
	return out, nil
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
				if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit; 0700 is owner only.
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
	info, err := os.Lstat(dir)
	if err != nil {
		return "", nil, err
	}
	if root.WritableByOthers(info) {
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
	res, err := createBackup(ctx, o)
	if isBusy(err) {
		retry := "local-base --root '" + o.Root.Dir + "' backup"
		if o.DryRun {
			retry += " --dry-run"
		}
		if o.ExcludeSecrets {
			retry += " --exclude-secrets"
		}
		if o.Output != "" {
			retry += " --output '" + o.Output + "'"
		}
		return nil, errors.New(busyMessage(o.Root.Dir, retry, err))
	}
	return res, err
}

func createBackup(ctx context.Context, o Options) (*Result, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	dbPath := o.Root.DB()
	if _, err := os.Lstat(dbPath); err != nil {
		return nil, fmt.Errorf("%s has no database to back up: %w", o.Root.Dir, err)
	}
	// Any other failure here shows at the step that needs the data, with that step's name.
	if err := retryBusy(ctx, func() error { return readableDatabase(ctx, dbPath) }); isUnfinishedWrite(err) {
		return nil, errors.New(unfinishedWriteMessage(o.Root.Dir))
	}
	files, left, err := survey(o.Root.Dir)
	if err != nil {
		return nil, err
	}
	var (
		secrets             *secretSet
		migrations          int
		rowsPresent, models []string
	)
	err = retryBusy(ctx, func() error {
		var err error
		if secrets, err = readSecrets(ctx, o.Root.Dir, o.ExcludeSecrets); err != nil {
			return err
		}
		if migrations, err = appliedMigrations(ctx, dbPath); err != nil {
			return err
		}
		if rowsPresent, err = presentRows(ctx, dbPath); err != nil {
			return err
		}
		models, err = selectedModels(ctx, dbPath)
		return err
	})
	if err != nil {
		return nil, err
	}
	m := &Manifest{Format: Format, CreatedAt: o.Now.UTC().Format(time.RFC3339), Version: o.Version, GoSchema: store.GoSchemaVersion(),
		Migrations: migrations, Secrets: "included", SecretNames: secrets.names, SecretsRemoved: secrets.removed, NotIncluded: left,
		RowsLeftOut: rowsPresent, Models: models, RestoreRules: []string{SignOutLine, HeldKeyLine, RevokedLine, ChangedKeyLine}}
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

	// A dry run does all of this too, so that its answer can be believed, and keeps nothing. It
	// works beside the root, since the place it would write may not exist yet.
	place := filepath.Dir(out)
	if o.DryRun {
		place = filepath.Dir(o.Root.Dir)
	}
	res.Cautions = append(res.Cautions, sweep(place, filepath.Dir(o.Root.Dir))...)
	stage, err := newTemporary(place, "backup")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
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
	if m.Issuers, m.Unbound, err = describeBindings(ctx, content); err != nil {
		return nil, err
	}
	archive := filepath.Join(stage, "archive")
	if err := writeArchive(ctx, archive, content, m); err != nil {
		return nil, err
	}
	// The real defence against backup and restore disagreeing: a file its own restore would refuse
	// is never kept, or reported as a backup.
	if err := selfCheck(ctx, archive, stage); err != nil {
		return nil, fmt.Errorf("This backup would not restore, so it was not kept: %w", err)
	}
	info, err := os.Stat(archive)
	if err != nil {
		return nil, err
	}
	res.Bytes = info.Size()
	if o.DryRun {
		return res, nil
	}
	if err := placeFile(archive, out); err != nil {
		return nil, err
	}
	return res, nil
}

// selfCheck reads a finished backup the way a restore does, into a directory that goes with the
// stage, with every check a restore makes before it changes anything.
func selfCheck(ctx context.Context, archive, stage string) error {
	in, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer in.Close()
	dir := filepath.Join(stage, "selfcheck")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	m, err := extract(ctx, in, dir)
	if err != nil {
		return err
	}
	_, err = validate(ctx, dir, filepath.Join(stage, "would-be-root"), m, &targetState{newRoot: true}, nil)
	return err
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
		_, err = io.Copy(tw, ctxReader{ctx, in})
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

// placeFile puts a finished backup at its name, and never over another file. A link fails if the
// name is taken; a filesystem without links gets an exclusive copy instead.
func placeFile(archive, out string) error {
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
