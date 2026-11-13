package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/safefile"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
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
			shown[i] = termsafe.Printable(x)
		case error:
			shown[i] = termsafe.Printable(x.Error())
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
	opts    options // (tests) for Commit and its undo
}

// Discard removes the staging directory. Commit does too.
func (s *Staged) Discard() { os.RemoveAll(s.dir) }

// Stage reads a backup into a private directory and checks all of it. It changes nothing
// else, so a dry run is Stage and Discard.
//
// bringBack names keys to take the backup's version of, where the root being replaced has its own.
func Stage(ctx context.Context, file, targetDir string, replace bool, bringBack ...string) (*Staged, error) {
	return stage(ctx, file, targetDir, replace, bringBack, options{})
}

// stage is Stage, with a test's options for the checks it makes of the target.
func stage(ctx context.Context, file, targetDir string, replace bool, bringBack []string, o options) (*Staged, error) {
	staged, err := stageRestore(ctx, file, targetDir, replace, bringBack, o)
	if isBusy(err) {
		retry := "local-base --root " + quote(targetDir) + " restore"
		if replace {
			retry += " --replace"
		}
		return nil, refuse("%s", busyMessage(targetDir, retry+" "+quote(file), err))
	}
	return staged, err
}

func stageRestore(ctx context.Context, file, targetDir string, replace bool, bringBack []string, o options) (*Staged, error) {
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
	state.opts = o
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
	in, err := safefile.OpenOwnedRegular(file, os.O_RDONLY)
	if err != nil {
		var unsafe *safefile.UnsafeFileError
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
