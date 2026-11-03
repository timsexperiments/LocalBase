package backup

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

// Refused is a backup or a target that restore will not use. Nothing has changed.
type Refused struct{ Why string }

func (e *Refused) Error() string { return e.Why }

func refuse(format string, a ...any) error { return &Refused{fmt.Sprintf(format, a...)} }

// Program is one custom backend's whole command line: the program can be a shell.
type Program struct {
	Backend string   `json:"backend"`
	Command []string `json:"command"`
	Exists  bool     `json:"exists"`
}

// ModelSource is an imported model that reads a local path or a direct URL.
type ModelSource struct {
	Model string `json:"model"`
	Kind  string `json:"kind"`
	Where string `json:"where"`
}

// Setting is one setting that changes exposure or where data goes.
type Setting struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// KeyCounts says what an API key count means. Most of a long-lived root's keys can be revoked, and
// "68 keys" alone reads as 68 that work.
type KeyCounts struct {
	Total   int `json:"total"`
	Active  int `json:"active"`
	Revoked int `json:"revoked"`
	Expired int `json:"expired"`
}

// Report is what a restore would do. It is shown before anything is asked.
type Report struct {
	Target    string        `json:"target"`
	NewRoot   bool          `json:"newRoot"`
	Manifest  *Manifest     `json:"manifest"`
	Rules     []string      `json:"rules"`
	Programs  []Program     `json:"programs"`
	Sources   []ModelSource `json:"modelSources"`
	Settings  []Setting     `json:"settings"`
	Providers []string      `json:"signInProviders"`
	Keys      KeyCounts     `json:"keys"`
	Users     int           `json:"users"`
	Ports     []int         `json:"ports"`
	// With a root being replaced, what the backup would bring back, and what stays as it is.
	RevocationsKept int      `json:"revocationsKept"`
	ReturningKeys   []string `json:"returningKeys"`
	KeysLost        []string `json:"keysLost"`
	ReturningUsers  int      `json:"returningUsers"`
	// A user the target has removed or disabled stays that way, as a revoked key does.
	UserStatusesKept int      `json:"userStatusesKept"`
	UsersChanged     int      `json:"usersGoingBackToAnOlderStatus"`
	Cautions         []string `json:"cautions"`
}

// Staged is a backup read and checked, in a private directory beside its target.
type Staged struct {
	Report  *Report
	dir     string
	target  string
	replace bool
}

// Discard removes the staging directory. Commit does too.
func (s *Staged) Discard() { os.RemoveAll(s.dir) }

// replaceable is all a restore moves, here and on undo. It is a constant, so a journal
// can't name anything else.
var replaceable = []string{"local-base.db", "local-base.db-wal", "local-base.db-shm", "local-base.db-journal", "ui-access.json", "email-delivery.json", "credentials", "catalog.d", "backends.d", "voices"}

// targetState is what a restore found at its target.
type targetState struct{ newRoot, hasData bool }

func checkTarget(target string, replace bool) (*targetState, error) {
	if home, _ := os.UserHomeDir(); target == string(filepath.Separator) || (home != "" && target == home) {
		return nil, refuse("Refusing to restore into %s.", target)
	}
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return &targetState{newRoot: true}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, refuse("%s is not a directory.", target)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		if !ownedByMe(info) {
			return nil, refuse("Refusing to use %s: another user owns it.", target)
		}
		return &targetState{newRoot: true}, nil
	}
	// Before opening it: another user's root may not even be readable, and "can't read the
	// marker" would hide what is wrong.
	if !ownedByMe(info) {
		return nil, refuse("Refusing to use %s: another user owns it. If it is yours, run: sudo chown -R \"$(id -un)\" '%s'. If you did not put it there, choose another directory.", target, target)
	}
	r, err := root.Open(target)
	if err != nil {
		var unsafe *root.UnsafeRootError
		if errors.As(err, &unsafe) {
			return nil, &Refused{err.Error()}
		}
		// Not a root, or a copy of one. --replace doesn't change this: it isn't ours to replace.
		return nil, refuse("%s holds something that is not this LocalBase root, so restore will not touch it: %v", target, err)
	}
	switch state, owner, detail := r.OwnerState(); state {
	case "active":
		pid := ""
		if owner != nil {
			pid = fmt.Sprintf(" (pid %d)", owner.PID)
		}
		return nil, refuse("A gateway%s is running on %s. Stop it first: local-base --root '%s' stop", pid, target, target)
	case "missing", "stale":
	default:
		return nil, refuse("The gateway lock on %s can't be read, so a gateway may be running: %s", target, detail)
	}
	s := &targetState{}
	for _, name := range replaceable {
		if _, err := os.Lstat(filepath.Join(target, name)); err == nil {
			s.hasData = true
		}
	}
	if s.hasData && !replace {
		return nil, refuse("%s already holds a LocalBase root. To replace its settings, run this again with --replace. Its models stay, and its old files are kept.", target)
	}
	return s, nil
}

// existingAncestor is the nearest directory that exists, for a target that doesn't yet.
func existingAncestor(p string) string {
	for {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// Stage reads a backup into a private directory and checks all of it. It changes nothing
// else, so a dry run is Stage and Discard.
func Stage(ctx context.Context, file, targetDir string, replace bool) (*Staged, error) {
	staged, err := stageRestore(ctx, file, targetDir, replace)
	if isBusy(err) {
		retry := "local-base --root '" + targetDir + "' restore"
		if replace {
			retry += " --replace"
		}
		return nil, refuse("%s", busyMessage(targetDir, retry+" '"+file+"'", err))
	}
	return staged, err
}

func stageRestore(ctx context.Context, file, targetDir string, replace bool) (*Staged, error) {
	if os.Getuid() == 0 {
		return nil, refuse("Refusing to restore as root: the files would belong to root, and the owner could not read them. Run this as the user who will own the root.")
	}
	target, err := root.Canonical(targetDir)
	if err != nil {
		return nil, err
	}
	state, err := checkTarget(target, replace)
	if err != nil {
		return nil, err
	}
	if !state.newRoot && state.hasData {
		// A database that is only busy is left for the comparison, which refuses it.
		read := func() error { return readableDatabase(ctx, filepath.Join(target, "local-base.db")) }
		if err := retryBusy(ctx, read); isUnfinishedWrite(err) {
			return nil, refuse("%s", unfinishedWriteMessage(target))
		}
	}
	// Before opening, since opening a file this way also tightens it, which would hide this.
	if info, err := os.Lstat(file); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o022 != 0 {
		return nil, refuse("Refusing to use %s: other users can change it, and restoring it decides who can sign in and what runs as you. If you trust it, run: chmod go-w '%s'", file, file)
	}
	in, err := obs.OpenOwnedRegular(file, os.O_RDONLY)
	if err != nil {
		var unsafe *obs.UnsafeFileError
		if errors.As(err, &unsafe) {
			return nil, &Refused{err.Error()}
		}
		return nil, err
	}
	defer in.Close()
	place := existingAncestor(filepath.Dir(target))
	swept := sweep(place)
	dir, err := newTemporary(place, "restore")
	if err != nil {
		return nil, err
	}
	s := &Staged{dir: dir, target: target, replace: replace}
	ok := false
	defer func() {
		if !ok {
			s.Discard()
		}
	}()
	m, err := extract(ctx, in, dir)
	if err != nil {
		return nil, err
	}
	report, err := validate(ctx, dir, target, m, state)
	if err != nil {
		return nil, err
	}
	report.Cautions = append(report.Cautions, swept...)
	report.Rules = []string{SignOutLine}
	if !report.NewRoot {
		report.Rules = append(report.Rules, RevokedLine)
	}
	if parent, err := os.Stat(filepath.Dir(target)); err == nil && parent.Mode().Perm()&0o002 != 0 {
		report.Cautions = append(report.Cautions, "Other users can write to "+filepath.Dir(target)+", which can swap this root for another. Owner and mode checks don't protect against that.")
	}
	s.Report = report
	ok = true
	return s, nil
}

var (
	sha256Hex  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	newerRe    = regexp.MustCompile(`^localbase\.backup/v[0-9]+$`)
	formerIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

func readManifest(tr *tar.Reader) (*Manifest, error) {
	h, err := tr.Next()
	if err != nil {
		return nil, refuse("This is not a LocalBase backup: %v", err)
	}
	if h.Name != manifestEntry || h.Typeflag != tar.TypeReg {
		return nil, refuse("This is not a LocalBase backup: it does not start with a manifest.")
	}
	if h.Size > maxManifestBytes {
		return nil, refuse("The manifest is larger than a backup's can be.")
	}
	raw, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes+1))
	if err != nil {
		return nil, refuse("The backup is damaged: %v", err)
	}
	if len(raw) > maxManifestBytes {
		return nil, refuse("The manifest is larger than a backup's can be.")
	}
	var probe struct {
		Format string `json:"format"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return nil, refuse("This is not a LocalBase backup: its manifest can't be read.")
	}
	if probe.Format != Format {
		if newerRe.MatchString(probe.Format) {
			return nil, refuse("This backup is in the format %s, and this version reads %s. Upgrade LocalBase to restore it.", probe.Format, Format)
		}
		return nil, refuse("This is not a LocalBase backup: unknown format %q.", probe.Format)
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, refuse("The manifest can't be read: %v", err)
	}
	return &m, nil
}

func checkManifestFiles(m *Manifest) (map[string]File, int64, error) {
	if len(m.Files) > maxEntries {
		return nil, 0, refuse("The backup lists more files than a backup can hold.")
	}
	byPath := map[string]File{}
	var total int64
	for _, f := range m.Files {
		if !holdable(f.Path) {
			return nil, 0, refuse("The backup lists a file a backup can't hold: %q.", f.Path)
		}
		if _, dup := byPath[f.Path]; dup {
			return nil, 0, refuse("The backup lists %q twice.", f.Path)
		}
		if f.Size < 0 || f.Size > maxFileBytes {
			return nil, 0, refuse("The backup lists %q with a size a backup can't hold.", f.Path)
		}
		if !sha256Hex.MatchString(f.SHA256) {
			return nil, 0, refuse("The backup lists %q with no usable checksum.", f.Path)
		}
		if total += f.Size; total > maxTotalBytes {
			return nil, 0, refuse("The backup is larger than a backup can be.")
		}
		byPath[f.Path] = f
	}
	if _, ok := byPath["local-base.db"]; !ok {
		return nil, 0, refuse("The backup holds no database.")
	}
	return byPath, total, nil
}

func checkRoom(dir string, need int64) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	if free := int64(st.Bavail) * int64(st.Bsize); free < need {
		return refuse("This needs %d bytes of disk, and %d are free.", need, free)
	}
	return nil
}

// extract reads a whole backup into dir. Every entry must be one the manifest lists, of the
// size and hash it says, and within limits that are constants.
func extract(ctx context.Context, in io.Reader, dir string) (*Manifest, error) {
	br := bufio.NewReader(in)
	gz, err := gzip.NewReader(br)
	if err != nil {
		return nil, refuse("This is not a LocalBase backup: %v", err)
	}
	defer func() { _ = gz.Close() }() // Only read from: a failed read has already refused the file.
	// One stream only: a second would be data after the end.
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	m, err := readManifest(tr)
	if err != nil {
		return nil, err
	}
	byPath, total, err := checkManifestFiles(m)
	if err != nil {
		return nil, err
	}
	if err := checkRoom(dir, total); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, refuse("The backup is damaged: %v", err)
		}
		if h.Typeflag != tar.TypeReg {
			return nil, refuse("The backup holds %q, which is not a plain file.", h.Name)
		}
		for key := range h.PAXRecords {
			if strings.HasPrefix(key, "GNU.sparse.") {
				return nil, refuse("The backup holds %q as a sparse file.", h.Name)
			}
		}
		if !strings.HasPrefix(h.Name, filesPrefix) {
			return nil, refuse("The backup holds %q, which it should not.", h.Name)
		}
		rel := strings.TrimPrefix(h.Name, filesPrefix)
		f, listed := byPath[rel]
		if !listed {
			return nil, refuse("The backup holds %q, which its manifest does not list.", h.Name)
		}
		if seen[rel] {
			return nil, refuse("The backup holds %q twice.", h.Name)
		}
		seen[rel] = true
		if h.Size != f.Size {
			return nil, refuse("%q is not the size the manifest says.", rel)
		}
		if err := writeEntry(ctxReader{ctx, tr}, filepath.Join(dir, filepath.FromSlash(rel)), f); err != nil {
			return nil, err
		}
	}
	for rel := range byPath {
		if !seen[rel] {
			return nil, refuse("The backup is missing %q, which its manifest lists.", rel)
		}
	}
	// A tar ends with zeros. Anything else, or another gzip stream, was added to the file.
	rest := bufio.NewReader(io.LimitReader(gz, 1<<20+1))
	var n int64
	for {
		b, err := rest.ReadByte()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, refuse("The backup is damaged: %v", err)
		}
		if n++; b != 0 || n > 1<<20 {
			return nil, refuse("The backup has data after its end.")
		}
	}
	if _, err := br.Peek(1); !errors.Is(err, io.EOF) {
		return nil, refuse("The backup has data after its end.")
	}
	return m, nil
}

func writeEntry(r io.Reader, to string, f File) error {
	if err := mkdirPrivate(filepath.Dir(to)); err != nil {
		return err
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := out.Chmod(0o600); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, f.Size+1))
	if errors.Is(err, context.Canceled) {
		return err
	}
	if err != nil {
		return refuse("The backup is damaged: %v", err)
	}
	if n != f.Size {
		return refuse("%q is not the size the manifest says.", f.Path)
	}
	if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return refuse("%q does not match its checksum.", f.Path)
	}
	return out.Sync()
}

// mkdirPrivate makes directories that are 0700 from their first moment, whatever the umask.
func mkdirPrivate(dir string) error {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return nil
	}
	if err := mkdirPrivate(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return os.Chmod(dir, 0o700) //nolint:gosec // G302: a directory needs its execute bit; 0700 is owner only.
}

// checkDatabase judges the file before anything runs against it: read-only, with the schema's
// functions untrusted, and before any migration. integrity_check alone would pass a trigger that
// adds a role binding after every policy change, and no report would show it.
func checkDatabase(ctx context.Context, dbPath string) error {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return refuse("The database can't be opened: %v", err)
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check(1)`).Scan(&result); err != nil {
		return refuse("The database is damaged: %v", err)
	}
	if result != "ok" {
		return refuse("The database is damaged: %s", result)
	}
	level, err := migrationLevel(ctx, db)
	if err != nil {
		return refuse("The database can't be read: %v", err)
	}
	known, err := store.MigrationCount()
	if err != nil {
		return err
	}
	if level > known {
		return refuse("A newer LocalBase wrote this backup: its database has %d migrations, and this version knows %d. Upgrade LocalBase to restore it.", level, known)
	}
	if hasTable(ctx, db, "go_schema") {
		var v int
		if err := db.QueryRowContext(ctx, `SELECT version FROM go_schema WHERE id = 1`).Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return refuse("The database can't be read: %v", err)
		}
		if v > store.GoSchemaVersion() {
			return refuse("A newer LocalBase wrote this backup: its database schema is %d, and this version knows %d. Upgrade LocalBase to restore it.", v, store.GoSchemaVersion())
		}
	}
	shape, err := store.ReadShape(ctx, db)
	if err != nil {
		return refuse("The database can't be read: %v", err)
	}
	expected, err := store.ExpectedShape(ctx, level)
	if err != nil {
		return err
	}
	if bad := shape.Unexpected(expected); len(bad) > 0 {
		return refuse("The database holds what LocalBase does not make, so it was changed by something else: %s.", strings.Join(bad, "; "))
	}
	return nil
}

// validate checks everything else, with the validators the gateway uses, and writes the staged
// database's fixes. It is the staged copy, and nothing else is touched.
func validate(ctx context.Context, dir, target string, m *Manifest, state *targetState) (*Report, error) {
	dbPath := filepath.Join(dir, "local-base.db")
	if err := checkDatabase(ctx, dbPath); err != nil {
		return nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, refuse("The database can't be used: %v", err)
	}
	defer func() { _ = st.Close() }() // Every write below has committed or returned its own error.
	st.DB.SetMaxOpenConns(1)
	// What a backup leaves out is removed, not trusted to be absent: a crafted file can carry
	// sessions, and a restore has to sign everyone out regardless.
	if err := dropRows(ctx, st.DB); err != nil {
		return nil, err
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE config SET root = ?`, target); err != nil {
		return nil, err
	}
	cfg, err := st.LoadConfig(ctx)
	if err != nil && !errors.Is(err, store.ErrNotConfigured) {
		return nil, refuse("The settings can't be read: %v", err)
	}
	// The gateway's own checks of roles, permissions, addresses and duplicates.
	policy, err := auth.LoadPolicy(ctx, st.DB)
	if err != nil {
		return nil, refuse("The access policy in this backup can't be read: %v", err)
	}
	if policy != nil {
		if err := policy.Normalize(); err != nil {
			return nil, refuse("The access policy in this backup is not valid: %v", err)
		}
	}

	rep := &Report{Target: target, NewRoot: state.newRoot, Manifest: m}
	access, err := auth.LoadAccess(dir)
	if err != nil {
		return nil, refuse("ui-access.json in this backup is not valid: %v", err)
	}
	if access != nil {
		for _, r := range access.Provider.Registrations {
			rep.Providers = append(rep.Providers, r.Kind+":"+r.ID)
		}
		if access.Provider.Kind != "direct" {
			rep.Providers = append(rep.Providers, access.Provider.Kind)
		}
	}
	if _, err := auth.LoadEmail(dir); err != nil {
		return nil, refuse("email-delivery.json in this backup is not valid: %v", err)
	}
	if _, err := acquire.LoadProfiles(dir); err != nil {
		return nil, refuse("The download credentials in this backup are not valid: %v", err)
	}
	if rep.Programs, err = checkBackends(dir); err != nil {
		return nil, err
	}
	if err := checkCatalog(dir, cfg); err != nil {
		return nil, err
	}
	if err := checkVoices(ctx, dir, st); err != nil {
		return nil, err
	}
	if rep.Sources, err = checkImported(ctx, st); err != nil {
		return nil, err
	}
	if err := fillCounts(ctx, st, rep); err != nil {
		return nil, err
	}
	if cfg != nil {
		rep.Ports = []int{cfg.GatewayPort, cfg.Port, cfg.SttPort}
		// A backend has no authentication, so it only ever listens on loopback, whatever its stored
		// host says. The report says where it will listen, not what is stored.
		rep.Settings = append(rep.Settings, Setting{"gateway, reachable from " + reach(cfg.GatewayHost), fmt.Sprintf("%s:%d", cfg.GatewayHost, cfg.GatewayPort)},
			Setting{"chat backend, reachable from this computer only", fmt.Sprintf("%s:%d", runtime.BackendHost(cfg.Host), cfg.Port)},
			Setting{"speech backend, reachable from this computer only", fmt.Sprintf("%s:%d", runtime.BackendHost(cfg.SttHost), cfg.SttPort)})
		if cfg.OtelEndpoint != "" {
			rep.Settings = append(rep.Settings, Setting{"OTLP endpoint: traces and logs go here", cfg.OtelEndpoint})
		}
		if cfg.OtelHeaders != "" {
			rep.Settings = append(rep.Settings, Setting{"OTLP headers", "set"})
		}
	}
	exposure, err := exposureSettings(ctx, st.DB)
	if err != nil {
		return nil, err
	}
	rep.Settings = append(rep.Settings, exposure...)
	if !state.newRoot && state.hasData {
		if err := compareWithTarget(ctx, st.DB, filepath.Join(target, "local-base.db"), rep); err != nil {
			return nil, err
		}
	} else {
		rep.Cautions = append(rep.Cautions, "Any key revoked since this backup was made works again here. Check them afterwards: local-base keys list")
	}
	return rep, nil
}

func checkBackends(dir string) ([]Program, error) {
	files, err := os.ReadDir(backends.Dir(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	builtin, err := backends.Load("")
	if err != nil {
		return nil, err
	}
	var out []Program
	for _, e := range files {
		d, err := backendProblem(filepath.Join(backends.Dir(dir), e.Name()), builtin)
		if err != nil {
			return nil, refuse("The backend %s in this backup is not valid: %v", e.Name(), err)
		}
		_, statErr := os.Stat(d.Launch.Command)
		out = append(out, Program{Backend: d.ID, Command: append([]string{d.Launch.Command}, d.Launch.Args...), Exists: statErr == nil})
	}
	return out, nil
}

// checkCatalog is what opening the root will do, so a root that would refuse every command is
// caught here.
func checkCatalog(dir string, cfg *store.Config) error {
	if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(formerLog))); err == nil {
		for _, id := range strings.Split(string(b), "\n") {
			if id = strings.TrimSpace(id); id != "" && !formerIDRe.MatchString(id) {
				return refuse("%s in this backup is not a list of ids.", formerLog)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	cat, err := catalog.LoadRoot(dir, nil)
	if err != nil {
		return refuse("The catalog in this backup is not valid: %v", err)
	}
	// The gateway only reports a bad entry. A backup leaves them out, so one here was put there.
	for _, d := range cat.Diagnostics {
		if d.Tier == "user" {
			return refuse("The catalog entry %s in this backup is not valid: %s", filepath.Base(d.Path), d.Message)
		}
	}
	if cfg == nil {
		return nil
	}
	conf.KeepFormerSelections(dir, cfg, cat)
	if issues := conf.ValidateModels(cfg, cat, false); len(issues) > 0 {
		return refuse("The settings in this backup are not valid: %s: %s", issues[0].Path, issues[0].Message)
	}
	return nil
}

func checkVoices(ctx context.Context, dir string, st *store.Store) error {
	rows, err := st.Voices(ctx)
	if err != nil {
		return refuse("The voices in this backup can't be read: %v", err)
	}
	for _, v := range rows {
		p, ok := voices.Path(dir, v.ID)
		if !ok {
			return refuse("A voice in this backup has an unusable id: %q.", v.ID)
		}
		f, err := describe(p, v.ID)
		if err != nil {
			return refuse("The voice %s is in this backup's database, and its recording is missing.", v.ID)
		}
		if f.SHA256 != strings.ToLower(v.SHA256) {
			return refuse("The recording of voice %s does not match its database row.", v.ID)
		}
	}
	return nil
}

// checkImported validates rows that the file validators don't reach: a definition is JSON that
// names files and where they come from.
func checkImported(ctx context.Context, st *store.Store) ([]ModelSource, error) {
	rows, err := st.ImportedModels(ctx)
	if err != nil {
		return nil, refuse("The imported models in this backup can't be read: %v", err)
	}
	var out []ModelSource
	for _, row := range rows {
		var m models.Model
		if err := json.Unmarshal(row.Definition, &m); err != nil {
			return nil, refuse("The imported model %q in this backup can't be read: %v", row.ID, err)
		}
		if m.ID != row.ID {
			return nil, refuse("The imported model %q in this backup has a definition for %q.", row.ID, m.ID)
		}
		if err := m.Validate(); err != nil {
			return nil, refuse("The imported model %q in this backup is not valid: %v", row.ID, err)
		}
		for _, v := range m.Variants {
			for _, a := range v.Artifacts {
				if !filepath.IsLocal(a.Filename) || strings.ContainsAny(a.Filename, `/\`) {
					return nil, refuse("The imported model %q in this backup names an unsafe file: %q.", row.ID, a.Filename)
				}
				switch a.Source.Kind {
				case "local":
					out = append(out, ModelSource{m.ID, "reads this path", a.Source.Path})
				case "url":
					out = append(out, ModelSource{m.ID, "downloads from", a.Source.URL})
				}
			}
		}
	}
	return out, nil
}

func fillCounts(ctx context.Context, st *store.Store, rep *Report) error {
	keys, err := st.ListAPIKeys(ctx)
	if err != nil {
		return refuse("The API keys in this backup can't be read: %v", err)
	}
	now := time.Now()
	for _, k := range keys {
		rep.Keys.Total++
		switch {
		case k.RevokedAt != nil:
			rep.Keys.Revoked++
		case k.Active(now):
			rep.Keys.Active++
		default:
			rep.Keys.Expired++
		}
	}
	if !hasTable(ctx, st.DB, "auth_users") {
		return nil
	}
	return st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_users`).Scan(&rep.Users)
}

// reach says plainly who could reach an address, for the report.
func reach(host string) string {
	bare := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(host), "["), "]")
	if runtime.BackendHost(host) == bare {
		return "this computer only"
	}
	return "other computers too"
}

// exposureSettings are stored settings that change who can reach this root, or where it reaches.
func exposureSettings(ctx context.Context, db *sql.DB) ([]Setting, error) {
	if !hasTable(ctx, db, "go_settings") {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM go_settings WHERE key LIKE 'imports.%' OR key LIKE 'gateway.%' ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Setting
	for rows.Next() {
		var s Setting
		if err := rows.Scan(&s.Name, &s.Value); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// comparedKey is what a restore compares of a key. These reads are of the root being replaced, where
// a failure must not look like finding nothing, which would bring a revoked key back.
type comparedKey struct {
	name    string
	revoked sql.NullString
}

func readComparedKeys(ctx context.Context, db *sql.DB) (map[string]comparedKey, error) {
	out := map[string]comparedKey{}
	present, err := tableExists(ctx, db, "api_keys")
	if err != nil || !present {
		return out, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id, name, revoked_at FROM api_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var k comparedKey
		if err := rows.Scan(&id, &k.name, &k.revoked); err != nil {
			return nil, err
		}
		out[id] = k
	}
	return out, rows.Err()
}

type comparedUser struct{ status, email string }

func readComparedUsers(ctx context.Context, db *sql.DB) (map[string]comparedUser, error) {
	out := map[string]comparedUser{}
	present, err := tableExists(ctx, db, "auth_users")
	if err != nil || !present {
		return out, err
	}
	emails, err := tableExists(ctx, db, "auth_user_emails")
	if err != nil {
		return nil, err
	}
	query := `SELECT id, status, '' FROM auth_users`
	if emails {
		query = `SELECT u.id, u.status, COALESCE(e.email, '') FROM auth_users u LEFT JOIN auth_user_emails e ON e.user_id = u.id`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var u comparedUser
		if err := rows.Scan(&id, &u.status, &u.email); err != nil {
			return nil, err
		}
		out[id] = u
	}
	return out, rows.Err()
}

// compareWithTarget keeps what the root being replaced has revoked, and reports what else an older
// backup would bring back. Restoring must not revive a key someone revoked because it leaked.
func compareWithTarget(ctx context.Context, staged *sql.DB, targetDB string, rep *Report) error {
	// hasTable reads an error as "no such table", which here would say that nothing is revoked. A
	// root is often replaced because its database is broken, so this can't refuse, but it says so.
	err := retryBusy(ctx, func() error { return readableDatabase(ctx, targetDB) })
	if isBusy(err) {
		return fmt.Errorf("reading this root's database: %w", err)
	}
	if err != nil {
		rep.Cautions = append(rep.Cautions, fmt.Sprintf("The database on this root can't be read (%v), so nothing could be compared with it. Any key revoked since this backup was made works again here, and anyone removed comes back. Check them afterwards: local-base keys list", err))
		return nil
	}
	targetReadHook()
	cur, err := openReadOnly(targetDB)
	if err != nil {
		return err
	}
	defer cur.Close()
	now, err := readComparedKeys(ctx, cur)
	if err != nil {
		return err
	}
	backed, err := readComparedKeys(ctx, staged)
	if err != nil {
		return err
	}
	for id, k := range backed {
		was, here := now[id]
		switch {
		case !here:
			if !k.revoked.Valid {
				rep.ReturningKeys = append(rep.ReturningKeys, k.name)
			}
		case was.revoked.Valid && !k.revoked.Valid:
			if _, err := staged.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE id = ?`, was.revoked.String, id); err != nil {
				return err
			}
			rep.RevocationsKept++
		}
	}
	for id, k := range now {
		if _, ok := backed[id]; !ok && !k.revoked.Valid {
			rep.KeysLost = append(rep.KeysLost, k.name)
		}
	}
	sort.Strings(rep.ReturningKeys)
	sort.Strings(rep.KeysLost)

	usersNow, err := readComparedUsers(ctx, cur)
	if err != nil {
		return err
	}
	usersBacked, err := readComparedUsers(ctx, staged)
	if err != nil {
		return err
	}
	// An id survives within one root's lineage. The address is the fallback.
	byEmail := map[string]comparedUser{}
	for _, u := range usersNow {
		if u.email != "" {
			byEmail[u.email] = u
		}
	}
	for id, u := range usersBacked {
		was, here := usersNow[id]
		if !here && u.email != "" {
			was, here = byEmail[u.email]
		}
		switch {
		case !here:
			if u.status != "removed" {
				rep.ReturningUsers++
			}
		case was.status == u.status:
		case was.status == "removed" || was.status == "disabled":
			if _, err := staged.ExecContext(ctx, `UPDATE auth_users SET status = ? WHERE id = ?`, was.status, id); err != nil {
				return err
			}
			rep.UserStatusesKept++
		default:
			rep.UsersChanged++
		}
	}
	return nil
}

// --- changing the root ---------------------------------------------------------------------------

// journal says what a restore in progress is doing. It is read back by --undo, as input: every
// path is built from a constant list and these validated names.
type journal struct {
	Version int      `json:"version"`
	Before  string   `json:"before"`
	Staging string   `json:"staging"`
	Had     []string `json:"had"`
	NewRoot bool     `json:"newRoot"`
}

var (
	beforeRe = regexp.MustCompile(`^\.before-restore-[0-9]{8}-[0-9]{6}(-[0-9]+)?$`)
	// stepHook is for tests: an error is a failure, and a panic is a crash.
	stepHook = func(string) error { return nil }
	// targetReadHook is for tests: a writer that arrives just after the first read.
	targetReadHook = func() {}
	// dirSync is for tests: a failing disk can't be arranged any other way.
	dirSync = func(d *os.File) error { return d.Sync() }
)

func journalPath(dir string) string { return filepath.Join(dir, root.RestoreJournalName) }

// syncDir makes the renames in a directory last through a power cut. A file system that can't do
// that for a directory says so with EINVAL or ENOTSUP, and there is nothing more to wait for there.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := dirSync(d); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// Done is what a finished restore left.
type Done struct {
	// BeforeDir holds the old files, which hold secrets too. It is empty for a new root.
	BeforeDir string `json:"beforeDir,omitempty"`
}

// Commit makes the staged backup the root's. It holds the operation lease and the gateway lock
// throughout, so no gateway can start midway. A failure puts everything back; a crash leaves the
// journal, and root.Open refuses until --undo.
func (s *Staged) Commit() (*Done, error) {
	defer s.Discard()
	lease, err := root.AcquireOperation(s.target, "restore")
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	state, err := checkTarget(s.target, s.replace)
	if err != nil {
		return nil, err
	}
	r, err := root.Init(s.target)
	if err != nil {
		return nil, err
	}
	if st, _, _ := r.OwnerState(); st == "stale" {
		if err := r.ClearStale(); err != nil {
			return nil, err
		}
	}
	unlock, err := r.Lock("", 0)
	if err != nil {
		return nil, err
	}
	defer unlock()

	stamp := beforePrefix + time.Now().UTC().Format("20060102-150405")
	j := &journal{Version: 1, Before: stamp, NewRoot: state.newRoot}
	if filepath.Dir(s.dir) == filepath.Dir(r.Dir) {
		j.Staging = filepath.Base(s.dir)
	}
	for n := 1; exists(filepath.Join(r.Dir, j.Before)); n++ {
		j.Before = fmt.Sprintf("%s-%d", stamp, n)
	}
	for _, name := range replaceable {
		if exists(filepath.Join(r.Dir, name)) {
			j.Had = append(j.Had, name)
		}
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return nil, err
	}
	if err := obs.WritePrivateAtomic(journalPath(r.Dir), raw); err != nil {
		return nil, err
	}
	if err := syncDir(r.Dir); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}

	if err := s.apply(r.Dir, j); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	if err := verify(r.Dir); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	if err := stepHook("finish"); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	if err := os.Remove(journalPath(r.Dir)); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	// It has finished. If this removal is lost in a power cut the journal comes back, the root
	// refuses to open, and the person is told to run the undo. Nothing is damaged.
	_ = syncDir(r.Dir)
	done := &Done{}
	before := filepath.Join(r.Dir, j.Before)
	if os.Remove(before) != nil { // it holds the old files, or removing an empty one failed
		done.BeforeDir = before
	}
	return done, nil
}

func (s *Staged) failed(dir string, j *journal, cause error) error {
	if err := undo(dir, j); err != nil {
		return fmt.Errorf("the restore failed (%v), and so did putting things back (%w). Run: local-base --root '%s' restore --undo", cause, err, dir)
	}
	return fmt.Errorf("the restore failed, and everything was put back: %w", cause)
}

func (s *Staged) apply(dir string, j *journal) error {
	before := filepath.Join(dir, j.Before)
	if err := stepHook("before"); err != nil {
		return err
	}
	if err := os.Mkdir(before, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(before, 0o700); err != nil { //nolint:gosec // G302: a directory needs its execute bit; 0700 is owner only.
		return err
	}
	for _, name := range j.Had {
		if err := stepHook("aside " + name); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(before, name)); err != nil {
			return err
		}
	}
	// The database last: until it is here, nothing looks like a restored root.
	order := append(append([]string{}, replaceable[1:]...), replaceable[0])
	for _, name := range order {
		from := filepath.Join(s.dir, name)
		if !exists(from) {
			continue
		}
		if err := stepHook("in " + name); err != nil {
			return err
		}
		if err := os.Rename(from, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

// verify gives the restored root the checks a gateway gets.
func verify(dir string) error {
	if _, err := root.OpenForUndo(dir); err != nil {
		return err
	}
	if bad := obs.CheckRootFiles(dir); len(bad) > 0 {
		return bad[0]
	}
	st, err := store.Open(filepath.Join(dir, "local-base.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }() // Only read from here.
	cfg, err := st.LoadConfig(context.Background())
	if errors.Is(err, store.ErrNotConfigured) {
		return nil
	}
	if err != nil {
		return err
	}
	if cfg.Root != dir {
		return fmt.Errorf("the restored settings name %q as their root", cfg.Root)
	}
	return nil
}

// undo puts a root back. It only ever moves names on the constant list, and is safe to run again.
func undo(dir string, j *journal) error {
	before := filepath.Join(dir, j.Before)
	for _, name := range replaceable {
		old, here := filepath.Join(before, name), filepath.Join(dir, name)
		switch {
		case exists(old):
			// It was moved aside, so anything at its name now is new.
			if err := os.RemoveAll(here); err != nil {
				return err
			}
			if err := os.Rename(old, here); err != nil {
				return err
			}
		case !contains(j.Had, name):
			// It was never there, so anything at its name now is new.
			if err := os.RemoveAll(here); err != nil {
				return err
			}
		}
		// Otherwise it hasn't moved yet, and is still the old one.
	}
	if err := os.Remove(before); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if j.Staging != "" {
		os.RemoveAll(filepath.Join(filepath.Dir(dir), j.Staging))
	}
	// The moves reach the disk while the journal still exists, so a failure here can be run again.
	if err := syncDir(dir); err != nil {
		return err
	}
	if err := os.Remove(journalPath(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Everything is back. If this removal is lost in a power cut the journal returns, and running
	// the undo again is safe.
	_ = syncDir(dir)
	if j.NewRoot {
		// This made the root, so it goes too, if that leaves nothing. It is never removed
		// with its contents: a journal must not be able to delete a root.
		os.Remove(filepath.Join(dir, ".localbase-root.json"))
		os.RemoveAll(filepath.Join(dir, "runtime", "gateway.lock"))
		os.Remove(filepath.Join(dir, "runtime"))
		os.Remove(dir)
	}
	return nil
}

// readJournal treats the journal as input: it may have been left by a crash, or by someone else.
func readJournal(dir string) (*journal, error) {
	raw, err := obs.ReadOwnedRegular(journalPath(dir))
	if err != nil {
		return nil, err
	}
	var j journal
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return nil, refuse("The restore journal can't be read: %v", err)
	}
	if j.Version != 1 || !beforeRe.MatchString(j.Before) {
		return nil, refuse("The restore journal names something a restore doesn't make: %q.", j.Before)
	}
	if j.Staging != "" && !temporaryRe.MatchString(j.Staging) {
		return nil, refuse("The restore journal names something a restore doesn't make: %q.", j.Staging)
	}
	for _, name := range j.Had {
		if !contains(replaceable, name) {
			return nil, refuse("The restore journal names something a restore doesn't touch: %q.", name)
		}
	}
	before := filepath.Join(dir, j.Before)
	if info, err := os.Lstat(before); err == nil {
		if !info.IsDir() || !ownedByMe(info) || info.Mode().Perm() != 0o700 {
			return nil, refuse("%s is not a private directory of yours, so it won't be moved back.", before)
		}
		for _, name := range replaceable {
			if info, err := os.Lstat(filepath.Join(before, name)); err == nil && (info.Mode()&fs.ModeSymlink != 0 || !ownedByMe(info)) {
				return nil, refuse("%s is a link or belongs to someone else, so it won't be moved back.", filepath.Join(before, name))
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if j.Staging != "" {
		if info, err := os.Lstat(filepath.Join(filepath.Dir(dir), j.Staging)); err == nil && (!info.IsDir() || !ownedByMe(info)) {
			j.Staging = "" // not ours to remove
		}
	}
	return &j, nil
}

// Undo puts back a root that a crash left half restored.
func Undo(targetDir string) error {
	if os.Getuid() == 0 {
		return refuse("Refusing to undo a restore as root. Run this as the user who owns the root.")
	}
	r, err := root.OpenForUndo(targetDir)
	if err != nil {
		return err
	}
	if !exists(journalPath(r.Dir)) {
		return refuse("There is no unfinished restore on %s.", r.Dir)
	}
	lease, err := root.AcquireOperation(r.Dir, "undo restore")
	if err != nil {
		return err
	}
	defer lease.Release()
	j, err := readJournal(r.Dir)
	if err != nil {
		return err
	}
	switch state, _, detail := r.OwnerState(); state {
	case "active":
		return refuse("A process still holds the gateway lock on %s. Stop it first.", r.Dir)
	case "stale":
		if err := r.ClearStale(); err != nil {
			return err
		}
	case "missing":
	default:
		return refuse("The gateway lock on %s can't be read: %s", r.Dir, detail)
	}
	return undo(r.Dir, j)
}
