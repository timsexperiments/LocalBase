package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

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
	opts             options // (tests) for the checks a stage makes
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
