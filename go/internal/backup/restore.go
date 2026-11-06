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
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
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

// refuse says why. A refusal can quote what's in the file, so what it quotes can't act on a terminal.
func refuse(format string, a ...any) error {
	shown := make([]any, len(a))
	for i, v := range a {
		switch x := v.(type) {
		case string:
			shown[i] = cli.Printable(x)
		case error:
			shown[i] = cli.Printable(x.Error())
		default:
			shown[i] = v
		}
	}
	return &Refused{fmt.Sprintf(format, shown...)}
}

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

// KeyChange is one API key a restore treats as something other than the same, named by the id that
// identifies it and the name a person gave it. It holds nothing of the secret.
type KeyChange struct {
	ID   string `json:"id"`
	Name string `json:"name" report:"letters"`
	Why  string `json:"why,omitempty"`
}

// Report is what a restore would do. It is shown before anything is asked.
type Report struct {
	Target   string    `json:"target" report:"letters"`
	NewRoot  bool      `json:"newRoot"`
	Manifest *Manifest `json:"manifest"`
	// Secrets are what the file holds, read from what it holds: its manifest's own list isn't believed.
	Secrets []string `json:"secrets"`
	// SecretsUnlisted are held and left off the manifest's own list.
	SecretsUnlisted []string `json:"secretsUnlisted,omitempty"`
	// PutBack are what the file says --exclude-secrets left out, each with a command this version built
	// from fields it checked, or none.
	PutBack []PutBack `json:"putBack,omitempty"`
	// Catalog are the catalog.d entries this root's catalog will trust as its own, by id.
	Catalog []CatalogEntry `json:"catalog,omitempty"`
	// Downloads are the download credentials in it and where each will be sent, by id. Header names only.
	Downloads []DownloadCredential `json:"downloadCredentials,omitempty"`
	Rules     []string             `json:"rules"`
	Programs  []Program            `json:"programs"`
	Sources   []ModelSource        `json:"modelSources"`
	Settings  []Setting            `json:"settings"`
	// OwnSettings change what the root does by itself, shown by what they do and set against the root
	// being replaced. Both are always there, since a line left out would read as safe.
	OwnSettings []OwnSetting `json:"ownSettings"`
	Providers   []string     `json:"signInProviders"`
	SignIn      *SignIn      `json:"signIn"`
	Grants      []Grant      `json:"grants"`
	// RolesGone are the roles this root has and the backup doesn't.
	RolesGone []Grant   `json:"rolesGone,omitempty"`
	Keys      KeyCounts `json:"keys"`
	Users     int       `json:"users"`
	Ports     []int     `json:"ports"`
	// Who a provider's bindings cover, by issuer, and those that name no provider.
	Issuers     []IssuerLine      `json:"issuers"`
	Unbound     auth.UnboundCount `json:"unbound"`
	UnboundNote string            `json:"unboundNote,omitempty"`
	// With a root being replaced, what the backup would bring back, and what stays as it is.
	ReturningBindings []IssuerLine `json:"returningBindings,omitempty"`
	RevocationsKept   int          `json:"revocationsKept"`
	// KeysKept have changed on the root being replaced, and stay as they are there.
	KeysKept []KeyChange `json:"keysKept"`
	// KeysLeftOut are active in the backup and not on that root, and stay out.
	KeysLeftOut []KeyChange `json:"keysLeftOut"`
	// KeysBroughtBack are those a person named, to take the backup's version of.
	KeysBroughtBack []KeyChange `json:"keysBroughtBack"`
	// KeysLost are active on that root and not in the backup, and stop working.
	KeysLost []KeyChange `json:"keysLost"`
	// KeysActive are what would work, when there's no root's keys to set them against.
	KeysActive []KeyChange `json:"keysActive"`
	// KeysHeldBack can do more than make requests, have no root's keys to set them against, and stay out.
	KeysHeldBack   []KeyChange `json:"keysHeldBack"`
	ReturningUsers int         `json:"returningUsers"`
	// A user the target has removed or disabled stays that way, as a revoked key does.
	UserStatusesKept int `json:"userStatusesKept"`
	UsersChanged     int `json:"usersGoingBackToAnOlderStatus"`
	// Cautions keep their letters because they hold this person's own path, often as a command to copy.
	// So a caution mustn't quote the file: what the file says goes in a field of its own.
	Cautions []string `json:"cautions" report:"letters"`
}

// Staged is a backup read and checked, in a private directory beside its target.
type Staged struct {
	Report  *Report
	dir     string
	target  string
	replace bool
	// live is what the report said about the root being replaced, read before it was written. It's nil
	// for a new root, an empty one, or one that couldn't be read, which have nothing to hold it to.
	live *liveFacts
	// hadData says the report was about a root with something to replace.
	hadData bool
	// release lets go of the root's database, once Commit holds it.
	release func()
}

// Discard removes the staging directory. Commit does too.
func (s *Staged) Discard() { os.RemoveAll(s.dir) }

// replaceable is all a restore moves, here and on undo. It never changes, so a journal can't name
// anything else. The database is first, and goes in last. Whatever belongs with a database moves with it,
// because a -wal left beside another database is replayed into it, and nothing notices.
var replaceable = func() []string {
	names := []string{"local-base.db"}
	for _, suffix := range store.Sidecars {
		names = append(names, "local-base.db"+suffix)
	}
	return append(names, "ui-access.json", "email-delivery.json", "credentials", "catalog.d", "backends.d", "voices")
}()

// targetState is what a restore found at its target.
type targetState struct {
	newRoot, hasData bool
	// liveDB is the file to read the root's database through, for a root being replaced. With liveErr,
	// it couldn't be read at all, which a root is often replaced because of, so this doesn't refuse.
	liveDB  string
	liveErr error
}

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
		note := root.DefaultRootNote(target)
		// `stop` only controls the service, and refuses a gateway started with serve.
		if owner != nil && owner.ServiceID == "" {
			return nil, refuse("A gateway%s started with local-base serve is running on %s.%s Stop it where it is running, with Ctrl-C or by ending that process. The service commands don't control it.", pid, target, note)
		}
		// Restoring needs it stopped, so this names the command, and what else that does.
		return nil, refuse("A gateway%s is running on %s.%s Stop it first: local-base --root %s stop. That also disables the service, until you run: local-base --root %s start", pid, target, note, quote(target), quote(target))
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
//
// bringBack names keys to take the backup's version of, where the root being replaced has its own.
func Stage(ctx context.Context, file, targetDir string, replace bool, bringBack ...string) (*Staged, error) {
	staged, err := stageRestore(ctx, file, targetDir, replace, bringBack)
	if isBusy(err) {
		retry := "local-base --root " + quote(targetDir) + " restore"
		if replace {
			retry += " --replace"
		}
		return nil, refuse("%s", busyMessage(targetDir, retry+" "+quote(file), err))
	}
	return staged, err
}

func stageRestore(ctx context.Context, file, targetDir string, replace bool, bringBack []string) (*Staged, error) {
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
		liveDB, removeCopy, err := readRoot(ctx, filepath.Join(target, "local-base.db"))
		if errors.Is(err, store.ErrInUse) {
			return nil, err
		}
		defer removeCopy()
		state.liveDB, state.liveErr = liveDB, err
		if err == nil {
			// A database that is only busy is left for the comparison, which refuses it.
			read := func() error { return readableDatabase(ctx, liveDB) }
			if err := retryBusy(ctx, read); isUnfinishedWrite(err) {
				return nil, refuse("%s", unfinishedWriteMessage(target))
			}
		}
	}
	// Before opening, since opening a file this way also tightens it, which would hide this.
	if info, err := os.Lstat(file); err == nil && info.Mode().IsRegular() && root.WritableByOthers(info) {
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
	s := &Staged{dir: dir, target: target, replace: replace, hadData: state.hasData}
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
	// Before the report, so that a change made while it's written is seen as one, and never missed.
	if !state.newRoot && state.hasData && state.liveErr == nil {
		s.live, err = readRootFacts(ctx, state.liveDB, target)
		if isBusy(err) {
			return nil, fmt.Errorf("reading this root's database: %w", err)
		}
		// Any other failure is for the report, which says the root can't be read.
	}
	report, err := validate(ctx, dir, target, m, state, bringBack)
	if err != nil {
		return nil, err
	}
	report.Cautions = append(report.Cautions, swept...)
	report.Rules = []string{SignOutLine, HeldKeyLine}
	if !report.NewRoot {
		report.Rules = append(report.Rules, RevokedLine, ChangedKeyLine)
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
	var total, settings int64
	for _, f := range m.Files {
		if !holdable(f.Path) {
			return nil, 0, refuse("The backup lists a file a backup can't hold: %q.", f.Path)
		}
		if _, dup := byPath[f.Path]; dup {
			return nil, 0, refuse("The backup lists %q twice.", f.Path)
		}
		if f.Size < 0 || f.Size > maxBytesOf(f.Path) {
			return nil, 0, refuse("The backup lists %q with a size a backup can't hold.", f.Path)
		}
		if isSettingsFile(f.Path) {
			if settings += f.Size; settings > maxSettingsTotalBytes {
				return nil, 0, refuse("The backup lists more settings than a backup can hold.")
			}
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
	schema, err := tableExists(ctx, db, "go_schema")
	if err != nil {
		return refuse("The database can't be read: %v", err)
	}
	if schema {
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
	if err := checkDatabaseSize(ctx, db, shape); err != nil {
		return err
	}
	return checkForeignKeys(ctx, db)
}

// checkDatabaseSize refuses a database holding more than a root could, before anything reads it into
// memory. octet_length takes a value's size from its header, so a large value isn't read to find out
// that it's large.
func checkDatabaseSize(ctx context.Context, db *sql.DB, shape *store.Shape) error {
	for _, name := range slices.Sorted(maps.Keys(shape.Tables)) {
		table := shape.Tables[name]
		query := `SELECT COUNT(*)`
		for _, column := range table.Columns {
			query += `, COALESCE(MAX(octet_length(` + quoteName(column.Name) + `)), 0), COALESCE(SUM(octet_length(` + quoteName(column.Name) + `)), 0)`
		}
		values := make([]int64, 1+2*len(table.Columns))
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := db.QueryRowContext(ctx, query+` FROM `+quoteName(name)).Scan(targets...); err != nil {
			return refuse("The database can't be read: %v", err)
		}
		var total int64
		for i := range table.Columns {
			if largest := values[1+2*i]; largest > maxDatabaseValueBytes {
				return refuse("The database holds more than a root could, so it was changed by something else: a value of %d bytes in %s.", largest, name)
			}
			total += values[2+2*i]
		}
		if name == usageTable {
			continue
		}
		if values[0] > maxDatabaseRows {
			return refuse("The database holds more than a root could, so it was changed by something else: %d rows in %s.", values[0], name)
		}
		if total > maxDatabaseTableBytes {
			return refuse("The database holds more than a root could, so it was changed by something else: %d bytes in %s.", total, name)
		}
	}
	return nil
}

// settleJournalMode leaves the staged database as one file. How a database is journaled is the store's
// to decide, so a file doesn't get to choose it for the root by what its header claims, and nothing
// beside the database has to move with it. It's the last thing done to the staged database, so that
// whatever has been written is already in there.
func settleJournalMode(ctx context.Context, db *sql.DB) error {
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		return err
	}
	return modeAfterSettling(mode)
}

// journalModes are SQLite's own words for a journal mode, the only ones a refusal shows.
var journalModes = []string{"delete", "truncate", "persist", "memory", "wal", "off"}

// modeStayedError is a journal mode that didn't change. Its mode is always one of journalModes.
type modeStayedError struct{ mode string }

func (e *modeStayedError) Error() string { return "its journal mode stayed " + e.mode }

// modeAfterSettling says what SQLite's answer means. An answer that isn't one of its own words, which SQLite
// doesn't give, is said without it.
func modeAfterSettling(mode string) error {
	mode = strings.ToLower(mode)
	switch {
	case mode == "delete":
		return nil
	case slices.Contains(journalModes, mode):
		return &modeStayedError{mode}
	}
	return errors.New("its journal mode didn't change")
}

// settleJournal is settleJournalMode, as a variable so that a test can see a restore refuse when it can't be
// done, which no real file has been found to cause.
var settleJournal = settleJournalMode

// usageTable piles up, and a restore never reads it into memory, so only its values are limited.
const usageTable = "usage_hourly"

func quoteName(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// checkForeignKeys refuses a database with rows pointing at something that isn't there. LocalBase keeps
// foreign keys on, so only an edit of the file can make one, and what reads such a row may not look
// at what it points to. It runs after the schema is compared, so the foreign keys it checks are the
// ones this version makes, and the tables it names are too. It says how many rows, and nothing in them.
func checkForeignKeys(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return refuse("The database can't be read: %v", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var key int
		if err := rows.Scan(&table, &rowid, &parent, &key); err != nil {
			return refuse("The database can't be read: %v", err)
		}
		counts[table]++
	}
	if err := rows.Err(); err != nil {
		return refuse("The database can't be read: %v", err)
	}
	if len(counts) == 0 {
		return nil
	}
	var found []string
	for table, n := range counts {
		found = append(found, fmt.Sprintf("%s: %d", table, n))
	}
	slices.Sort(found)
	return refuse("The database has rows pointing at something that isn't there, so it was changed by something else. %s.", strings.Join(found, ", "))
}

// validate checks everything else, with the validators the gateway uses, and writes the staged
// database's fixes. It is the staged copy, and nothing else is touched.
func validate(ctx context.Context, dir, target string, m *Manifest, state *targetState, bringBack []string) (*Report, error) {
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

	rep := &Report{Target: target, NewRoot: state.newRoot, Manifest: manifestToShow(m)}
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
	// Checked as every use of them checks them. An origin the huggingface profile can't name is left out wherever
	// the profile is used, so it isn't a reason to refuse.
	profiles, err := acquire.LoadProfiles(dir)
	if err == nil {
		err = profiles.FirstInvalid()
	}
	if err != nil {
		return nil, refuse("The download credentials in this backup are not valid: %v", err)
	}
	if rep.Downloads, err = downloadCredentials(profiles); err != nil {
		return nil, err
	}
	if rep.Programs, err = checkBackends(dir); err != nil {
		return nil, err
	}
	cat, err := checkCatalog(dir, cfg)
	if err != nil {
		return nil, err
	}
	if rep.Catalog, err = catalogEntries(cat); err != nil {
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
	if err := checkBindingRows(ctx, st.DB); err != nil {
		return nil, err
	}
	backedSignIn := readSignIn(ctx, dir, st.DB)
	if backedSignIn.policyErr != nil {
		return nil, refuse("The access policy in this backup can't be read: %v", backedSignIn.policyErr)
	}
	var liveValues *signInValues
	if !state.newRoot && state.hasData {
		v := liveSignIn(ctx, target, state.liveDB, state.liveErr)
		liveValues = &v
	}
	var signInCaution string
	rep.SignIn, signInCaution = signInReport(backedSignIn, liveValues)
	if signInCaution != "" {
		rep.Cautions = append(rep.Cautions, signInCaution)
	}
	if rep.Grants, err = readGrants(ctx, st.DB); err != nil {
		return nil, refuse("The roles in this backup can't be read: %v", err)
	}
	if !state.newRoot && state.hasData {
		live, liveErr := liveGrants(ctx, state.liveDB, state.liveErr)
		gone, changed := compareGrants(rep.Grants, live, liveErr)
		rep.RolesGone = gone
		switch {
		case changed && liveErr != nil:
			rep.Cautions = append(rep.Cautions, grantsUnknownCaution)
		case changed:
			rep.Cautions = append(rep.Cautions, grantsChangedCaution)
		}
	}
	if rep.Issuers, rep.Unbound, err = bindingsOf(ctx, st.DB, access); err != nil {
		return nil, refuse("The access bindings in this backup can't be read: %v", err)
	}
	rep.UnboundNote = unboundNote(rep.Unbound, access)
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
	backedOwn, err := readOwnSettings(ctx, st.DB)
	if err != nil {
		return nil, refuse("The settings in this backup can't be read: %v", err)
	}
	// A root with no data has what a new one has, and it is set against that by name: "this root".
	liveOwn, liveOwnErr := map[string]string{}, error(nil)
	for _, o := range ownSettings {
		liveOwn[o.key] = o.byDefault
	}
	if !state.newRoot && state.hasData {
		liveOwn, liveOwnErr = liveOwnSettings(ctx, state.liveDB, state.liveErr)
	}
	var ownCaution string
	rep.OwnSettings, ownCaution = ownSettingsReport(backedOwn, liveOwn, liveOwnErr, state.newRoot)
	if ownCaution != "" {
		rep.Cautions = append(rep.Cautions, ownCaution)
	}
	compared := false
	if !state.newRoot && state.hasData {
		if compared, err = compareWithTarget(ctx, st.DB, state.liveDB, state.liveErr, rep, bringBack); err != nil {
			return nil, err
		}
	} else {
		if err := holdBackKeys(ctx, st.DB, rep, bringBack); err != nil {
			return nil, err
		}
		rep.Cautions = append(rep.Cautions, "Any key revoked or rotated since this backup was made works again here. Check them afterwards: local-base keys list")
	}
	for _, list := range [][]KeyChange{rep.KeysKept, rep.KeysLeftOut, rep.KeysBroughtBack, rep.KeysLost, rep.KeysHeldBack} {
		sortKeys(list)
	}
	if !compared {
		if rep.KeysActive, err = activeKeys(ctx, st); err != nil {
			return nil, err
		}
	}
	// What it holds, read the way a backup reads a root, once each file has passed its own check. A file
	// can say anything about itself.
	held, err := readSecrets(ctx, dir, dbPath, false)
	if err != nil {
		return nil, refuse("The secrets in this backup can't be read: %v", err)
	}
	rep.Secrets = held.names
	for _, name := range held.names {
		if !slices.Contains(m.SecretNames, name) {
			rep.SecretsUnlisted = append(rep.SecretsUnlisted, name)
		}
	}
	for _, r := range m.SecretsRemoved {
		rep.PutBack = append(rep.PutBack, putBack(r))
	}
	// This is the staged copy. Nothing has touched the root yet, whichever command is asking: a restore that's
	// still staging, or a backup checking that it would restore.
	if err := settleJournal(ctx, st.DB); err != nil {
		var stayed *modeStayedError
		if errors.As(err, &stayed) {
			return nil, refuse("This backup's database can't be used: its journal mode stayed %s, so nothing was changed.", stayed.mode)
		}
		return nil, refuse("The database can't be used: %v", err)
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
// caught here. It returns the catalog it loaded, for the report.
func checkCatalog(dir string, cfg *store.Config) (*catalog.Catalog, error) {
	if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(formerLog))); err == nil {
		for _, id := range strings.Split(string(b), "\n") {
			if id = strings.TrimSpace(id); id != "" && !formerIDRe.MatchString(id) {
				return nil, refuse("%s in this backup is not a list of ids.", formerLog)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	cat, err := catalog.LoadRoot(dir, nil)
	if err != nil {
		return nil, refuse("The catalog in this backup is not valid: %v", err)
	}
	// The gateway only reports a bad entry. A backup leaves them out, so one here was put there.
	for _, d := range cat.Diagnostics {
		if d.Tier == "user" {
			return nil, refuse("The catalog entry %s in this backup is not valid: %s", filepath.Base(d.Path), d.Message)
		}
	}
	if cfg == nil {
		return cat, nil
	}
	conf.KeepFormerSelections(dir, cfg, cat)
	if issues := conf.ValidateModels(cfg, cat, false); len(issues) > 0 {
		return nil, refuse("The settings in this backup are not valid: %s: %s", issues[0].Path, issues[0].Message)
	}
	return cat, nil
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
	present, err := tableExists(ctx, st.DB, "auth_users")
	if err != nil || !present {
		return err
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
	present, err := tableExists(ctx, db, "go_settings")
	if err != nil || !present {
		return nil, err
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

// comparedKey is what a restore compares of a key: everything that decides what it can do, including
// the hash of its secret. These reads are of the root being replaced, where a failure must not look
// like finding nothing, which would bring a revoked key back.
type comparedKey struct {
	name, prefix, hash, rotated string
	// scopes isn't set on a root older than key permissions, which can't say what its keys may do.
	scopes, expires, revoked sql.NullString
}

// readComparedKeys reads every key from any database, so that whatever else needs to compare keys
// reads the same facts.
func readComparedKeys(ctx context.Context, db *sql.DB) (map[string]comparedKey, error) {
	out := map[string]comparedKey{}
	present, err := tableExists(ctx, db, "api_keys")
	if err != nil || !present {
		return out, err
	}
	scopes, err := columnExists(ctx, db, "api_keys", "scopes")
	if err != nil {
		return nil, err
	}
	column := "NULL"
	if scopes {
		column = "scopes"
	}
	rows, err := db.QueryContext(ctx, `SELECT id, name, prefix, key_hash, `+column+`, last_rotated_at, expires_at, revoked_at FROM api_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var k comparedKey
		if err := rows.Scan(&id, &k.name, &k.prefix, &k.hash, &k.scopes, &k.rotated, &k.expires, &k.revoked); err != nil {
			return nil, err
		}
		out[id] = k
	}
	return out, rows.Err()
}

// differences says how a backup's copy of a key differs from the one on the root, in words.
func (k comparedKey) differences(backed comparedKey) []string {
	var why []string
	if k.revoked.Valid && !backed.revoked.Valid {
		why = append(why, revokedHere)
	}
	if !k.revoked.Valid && backed.revoked.Valid {
		why = append(why, "revoked in this backup and not on this root")
	}
	if k.hash != backed.hash || k.prefix != backed.prefix || k.rotated != backed.rotated {
		why = append(why, "rotated")
	}
	if k.scopes.Valid && k.scopes != backed.scopes {
		why = append(why, "permissions changed")
	}
	if k.expires != backed.expires {
		why = append(why, "expiry changed")
	}
	if k.name != backed.name {
		why = append(why, "renamed")
	}
	return why
}

// How a key revoked on this root, and not in the backup, differs. On the list of keys that stay as they
// are, "revoked" is what stays true. On the list of keys going back it's what changes, and "revoked" alone
// would read as if it stays that way.
const (
	revokedHere    = "revoked"
	revokedAndBack = "revoked on this root, and would work again"
)

// broughtBackWords says how a key named to come back differs. Revoked is always first, when it's there.
func broughtBackWords(why []string) string {
	words := slices.Clone(why)
	if len(words) > 0 && words[0] == revokedHere {
		words[0] = revokedAndBack
	}
	return strings.Join(words, ", ")
}

// scopeList is a key's permissions. A list that doesn't read is shown as it stands, which is also more
// than making requests.
func (k comparedKey) scopeList() []string {
	var scopes []string
	if err := json.Unmarshal([]byte(k.scopes.String), &scopes); err != nil {
		return []string{k.scopes.String}
	}
	return scopes
}

// beyondRequests lists what a key may do beyond what a new key gets, which the code calls no
// administration. Anything else counts, so a permission added later, or one only a file has heard of,
// is treated as the stronger kind.
func (k comparedKey) beyondRequests() []string {
	scopes := slices.DeleteFunc(k.scopeList(), func(s string) bool { return slices.Contains(auth.DefaultKeyScopes, s) })
	slices.Sort(scopes)
	return scopes
}

// permissions is what the report says a list of permissions comes to.
func permissions(scopes []string) string {
	if len(scopes) == 0 {
		return "no permissions"
	}
	return strings.Join(scopes, ", ")
}

// holdBack keeps a key that can do more than make requests out of a restore, unless it's named.
func holdBack(ctx context.Context, staged *sql.DB, id string, k comparedKey, beyond []string, named, used map[string]bool, rep *Report) error {
	if named[id] {
		used[id] = true
		rep.KeysBroughtBack = append(rep.KeysBroughtBack, KeyChange{id, k.name, "can do " + permissions(beyond)})
		return nil
	}
	if _, err := staged.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id); err != nil {
		return err
	}
	rep.KeysHeldBack = append(rep.KeysHeldBack, KeyChange{id, k.name, permissions(beyond)})
	return nil
}

// holdBackKeys is for a restore with no keys of a root's to set a backup's against: a new root, a root
// with no keys, or one that can't be read. An ordinary key comes, since getting its clients back is
// what a backup is for. A key that can do more doesn't, since this is just where a file would bring in
// an administrator's key unnoticed.
func holdBackKeys(ctx context.Context, staged *sql.DB, rep *Report, bringBack []string) error {
	backed, err := readComparedKeys(ctx, staged)
	if err != nil {
		return err
	}
	named, used := keyNames(bringBack), map[string]bool{}
	for id, k := range backed {
		if k.revoked.Valid {
			continue // A revoked key does nothing, here or not.
		}
		if beyond := k.beyondRequests(); len(beyond) > 0 {
			if err := holdBack(ctx, staged, id, k, beyond, named, used, rep); err != nil {
				return err
			}
		}
	}
	return checkKeyNames(bringBack, used)
}

func keyNames(bringBack []string) map[string]bool {
	named := map[string]bool{}
	for _, id := range bringBack {
		named[id] = true
	}
	return named
}

// checkKeyNames refuses a name that did nothing. It may be a mistyped id, with the real key left on its
// current secret.
func checkKeyNames(bringBack []string, used map[string]bool) error {
	for _, id := range bringBack {
		if !used[id] {
			return refuse("--bring-back-key %s: there's nothing to bring back. Either this backup doesn't hold that key, or it would change nothing here.", id)
		}
	}
	return nil
}

// activeKeys lists every key that would work, with what it may do, by the gateway's own reading.
func activeKeys(ctx context.Context, st *store.Store) ([]KeyChange, error) {
	keys, err := st.ListAPIKeys(ctx)
	if err != nil {
		return nil, refuse("The API keys in this backup can't be read: %v", err)
	}
	now := time.Now()
	var out []KeyChange
	for i := range keys {
		if keys[i].Active(now) {
			out = append(out, KeyChange{keys[i].ID, keys[i].Name, permissions(keys[i].Scopes)})
		}
	}
	sortKeys(out)
	return out, nil
}

func sortKeys(keys []KeyChange) {
	slices.SortFunc(keys, func(a, b KeyChange) int { return strings.Compare(a.ID, b.ID) })
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

// compareWithTarget keeps the keys of the root being replaced as they are, and says so. Restoring must
// not revive a key someone revoked or rotated because it leaked, and a file mustn't add one to a root
// that has its own. Keys named in bringBack take the backup's version, and the report names them. It
// says whether every key could be compared.
func compareWithTarget(ctx context.Context, staged *sql.DB, targetDB string, liveErr error, rep *Report, bringBack []string) (compared bool, err error) {
	// hasTable reads an error as "no such table", which here would say that nothing is revoked. A
	// root is often replaced because its database is broken, so this can't refuse, but it says so.
	err = liveErr
	if err == nil {
		err = retryBusy(ctx, func() error { return readableDatabase(ctx, targetDB) })
	}
	if isBusy(err) {
		return false, fmt.Errorf("reading this root's database: %w", err)
	}
	if err != nil {
		if err := holdBackKeys(ctx, staged, rep, bringBack); err != nil {
			return false, err
		}
		rep.Cautions = append(rep.Cautions, fmt.Sprintf("The database on this root can't be read (%v), so nothing could be compared with it. Any key revoked or rotated since this backup was made works again here, and anyone removed comes back. Check them afterwards: local-base keys list", err))
		return false, nil
	}
	targetReadHook()
	cur, err := openReadOnly(targetDB)
	if err != nil {
		return false, err
	}
	defer cur.Close()
	now, err := readComparedKeys(ctx, cur)
	if err != nil {
		return false, err
	}
	if compared, err = compareKeys(ctx, staged, now, rep, bringBack); err != nil {
		return false, err
	}
	if rep.ReturningBindings, err = returningBindings(ctx, staged, cur); err != nil {
		return false, err
	}

	usersNow, err := readComparedUsers(ctx, cur)
	if err != nil {
		return false, err
	}
	usersBacked, err := readComparedUsers(ctx, staged)
	if err != nil {
		return false, err
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
				return false, err
			}
			rep.UserStatusesKept++
		default:
			rep.UsersChanged++
		}
	}
	return compared, nil
}

// compareKeys sets the backup's keys against the root's, and says whether it could set all of them. A
// root with none has nothing to compare, as a new root doesn't. A root older than key permissions can't
// say what its keys may do.
func compareKeys(ctx context.Context, staged *sql.DB, now map[string]comparedKey, rep *Report, bringBack []string) (bool, error) {
	if len(now) == 0 {
		return false, holdBackKeys(ctx, staged, rep, bringBack)
	}
	backed, err := readComparedKeys(ctx, staged)
	if err != nil {
		return false, err
	}
	compared := true
	for _, k := range now {
		compared = compared && k.scopes.Valid
	}
	named, used := keyNames(bringBack), map[string]bool{}
	for id, k := range backed {
		was, here := now[id]
		switch {
		case !here:
			switch {
			case k.revoked.Valid: // A revoked key does nothing, here or not.
			case named[id]:
				used[id] = true
				rep.KeysBroughtBack = append(rep.KeysBroughtBack, KeyChange{id, k.name, "this root doesn't have it"})
			default:
				if _, err := staged.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id); err != nil {
					return false, err
				}
				rep.KeysLeftOut = append(rep.KeysLeftOut, KeyChange{id, k.name, permissions(k.scopeList())})
			}
		default:
			// A root that can't say what its key may do can't vouch for what a file says it may.
			if !was.scopes.Valid && !k.revoked.Valid {
				if beyond := k.beyondRequests(); len(beyond) > 0 {
					if err := holdBack(ctx, staged, id, k, beyond, named, used, rep); err != nil {
						return false, err
					}
					continue
				}
			}
			why := was.differences(k)
			if len(why) == 0 {
				continue
			}
			text := strings.Join(why, ", ")
			if named[id] {
				used[id] = true
				rep.KeysBroughtBack = append(rep.KeysBroughtBack, KeyChange{id, k.name, broughtBackWords(why)})
				continue
			}
			// What the root has wins, whole, so a key can't come back partly.
			if _, err := staged.ExecContext(ctx, `UPDATE api_keys SET name = ?, prefix = ?, key_hash = ?, scopes = COALESCE(?, scopes), last_rotated_at = ?, expires_at = ?, revoked_at = ? WHERE id = ?`,
				was.name, was.prefix, was.hash, was.scopes, was.rotated, was.expires, was.revoked, id); err != nil {
				return false, err
			}
			rep.KeysKept = append(rep.KeysKept, KeyChange{id, was.name, text})
			if was.revoked.Valid && !k.revoked.Valid {
				rep.RevocationsKept++
			}
		}
	}
	if err := checkKeyNames(bringBack, used); err != nil {
		return false, err
	}
	for id, k := range now {
		if _, ok := backed[id]; !ok && !k.revoked.Valid {
			rep.KeysLost = append(rep.KeysLost, KeyChange{ID: id, Name: k.name})
		}
	}
	return compared, nil
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
	s.release = func() {}
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
	if state.hasData && !s.hadData {
		return nil, refuse("This root changed while this waited: it holds settings now, and the report said it had none. Nothing was changed. Run this again to see what it would do now.")
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

	release, err := s.holdRoot(context.Background(), r.Dir, r.DB())
	if err != nil {
		return nil, s.stopped(r.Dir, j, err)
	}
	s.release = release
	defer release()
	// Taking the lock can change which working files there are, and the journal has to say what's here.
	j.Had = nil
	for _, name := range replaceable {
		if exists(filepath.Join(r.Dir, name)) {
			j.Had = append(j.Had, name)
		}
	}
	if raw, err = json.Marshal(j); err != nil {
		return nil, s.failed(r.Dir, j, err)
	}
	if err := obs.WritePrivateAtomic(journalPath(r.Dir), raw); err != nil {
		return nil, s.failed(r.Dir, j, err)
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
	// Closing it deletes whatever journal is at its path, even with its own database moved away, so that
	// happens while this restore's journal still keeps every command out.
	release()
	if err := stepHook("released"); err != nil {
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

// stopped ends a restore that hasn't moved anything. A refusal says why, in its own words, and anything
// else is a failure like any other.
func (s *Staged) stopped(dir string, j *journal, cause error) error {
	var refused *Refused
	if !errors.As(cause, &refused) {
		return s.failed(dir, j, cause)
	}
	// Only the journal: nothing moved, and whatever else is here may be another process's working files.
	if err := os.Remove(journalPath(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%v And its journal couldn't be removed (%w). Run: local-base --root '%s' restore --undo", cause, err, dir)
	}
	_ = syncDir(dir) // If this removal is lost in a power cut the journal returns, and the undo is safe.
	return cause
}

func (s *Staged) failed(dir string, j *journal, cause error) error {
	if err := undo(dir, j, s.release); err != nil {
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
// release lets go of the root's database, for a restore that still holds it.
func undo(dir string, j *journal, release func()) error {
	before := filepath.Join(dir, j.Before)
	// Nothing new goes in until everything old is aside, so before that, whatever is at a name the journal
	// doesn't list isn't this restore's. It may be another process's working files, with writes in them,
	// or a settings file a command just made, and it stays. This has to be worked out before anything goes
	// back, and it's the same on a second run that had finished removing what was new.
	allAside := exists(before)
	for _, name := range j.Had {
		allAside = allAside && exists(filepath.Join(before, name))
	}
	if allAside {
		for _, name := range replaceable {
			if contains(j.Had, name) {
				continue
			}
			if err := stepHook("undo remove " + name); err != nil {
				return err
			}
			// It was never there, so anything at its name now is new.
			if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	// The database goes back last, as it goes in last: if this stops partway, a later run finds it still
	// aside, along with whatever else hasn't gone back yet.
	for _, name := range append(append([]string{}, replaceable[1:]...), replaceable[0]) {
		old, here := filepath.Join(before, name), filepath.Join(dir, name)
		if !exists(old) {
			continue // It hasn't moved, or it's already back.
		}
		if err := stepHook("undo back " + name); err != nil {
			return err
		}
		// It was moved aside, so anything at its name now is new.
		if err := os.RemoveAll(here); err != nil {
			return err
		}
		if err := os.Rename(old, here); err != nil {
			return err
		}
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
	// As in Commit: closing it deletes whatever journal is at its path, so that comes before anyone can start.
	release()
	if err := stepHook("undo released"); err != nil {
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
	return undo(r.Dir, j, func() {})
}

// manifestToShow is the manifest as a report shows it, --json included: without the commands the file gives
// for its removed secrets. Anything reading the report couldn't tell those from ours, and PutBack carries the
// checked ones.
func manifestToShow(m *Manifest) *Manifest {
	shown := *m
	shown.SecretsRemoved = nil
	for _, r := range m.SecretsRemoved {
		r.Restore = ""
		shown.SecretsRemoved = append(shown.SecretsRemoved, r)
	}
	return &shown
}
