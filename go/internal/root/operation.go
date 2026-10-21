package root

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// Root operations (TS service/ownership.ts): commands that change a root
// (configure, service start, access and model management) hold a lease, the
// directory <coordination>/<rootHash>.operation.lock with an owner.json. A
// short mutation lock (<lease>.mutation, taken by renaming a candidate
// directory into place) serializes edits of the owner record. The format is
// the TS one, so the Go and TS CLIs exclude each other.

const handoffTTL = 10 * time.Second

type operationOwner struct {
	Version          int    `json:"version"`
	OperationID      string `json:"operationId"`
	Operation        string `json:"operation"`
	Root             string `json:"root"`
	RootHash         string `json:"rootHash"`
	PID              int    `json:"pid"`
	StartedAt        string `json:"startedAt"`
	HandoffToken     string `json:"handoffToken,omitempty"`
	HandoffExpiresAt string `json:"handoffExpiresAt,omitempty"`
}

func (o *operationOwner) valid() bool {
	return o.Version == 1 && o.OperationID != "" && o.Operation != "" && len(o.Operation) <= 64 && o.Root != "" &&
		o.PID > 0 && o.StartedAt != "" && (o.HandoffToken == "") == (o.HandoffExpiresAt == "")
}

type mutationOwner struct {
	Version int    `json:"version"`
	Token   string `json:"token"`
	PID     int    `json:"pid"`
}

// CoordinationDir is where operation leases live: $XDG_RUNTIME_DIR/local-base,
// else a per-user runtime directory.
func CoordinationDir() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" && filepath.IsAbs(v) {
		return filepath.Join(v, "local-base")
	}
	home := os.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "LocalBase", "runtime")
	}
	return filepath.Join(home, ".local", "state", "local-base", "runtime")
}

type opPaths struct{ dir, lock, owner, mutation string }

func operationPaths(canonicalRoot string) opPaths {
	dir := CoordinationDir()
	lock := filepath.Join(dir, Hash(canonicalRoot)+".operation.lock")
	return opPaths{dir, lock, filepath.Join(lock, "owner.json"), lock + ".mutation"}
}

// presence: 1 present, 0 absent, -1 unknown (EPERM).
func presence(pid int) int {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return 1
	case errors.Is(err, syscall.ESRCH):
		return 0
	}
	return -1
}

func readJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(b, v) == nil
}

func writeJSONAtomic(path string, v any) error {
	tmp := path + "." + uuid() + ".tmp"
	defer os.Remove(tmp)
	b, _ := json.Marshal(v)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func isDir(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.IsDir()
}

func ensurePrivateDir(p string) error {
	if err := os.MkdirAll(p, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("Service coordination path is not a private directory: %s.", p)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("Service coordination path is not owned by the current user: %s.", p)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return os.Chmod(p, 0o700)
	}
	return nil
}

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// withMutation runs fn holding the owner-record mutation lock.
func withMutation(p opPaths, fn func() error) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		token := uuid()
		candidate := p.mutation + "." + token + ".candidate"
		if err := os.Mkdir(candidate, 0o700); err != nil {
			return err
		}
		if err := writeJSONAtomic(filepath.Join(candidate, "owner.json"), mutationOwner{1, token, os.Getpid()}); err != nil {
			os.RemoveAll(candidate)
			return err
		}
		if err := os.Rename(candidate, p.mutation); err == nil {
			defer func() {
				var o mutationOwner
				if readJSON(filepath.Join(p.mutation, "owner.json"), &o) && o.Token == token {
					os.RemoveAll(p.mutation)
				}
			}()
			return fn()
		} else if !errors.Is(err, os.ErrExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			os.RemoveAll(candidate)
			return err
		}
		os.RemoveAll(candidate)
		var o mutationOwner
		if readJSON(filepath.Join(p.mutation, "owner.json"), &o) && o.Version == 1 && o.PID > 0 && presence(o.PID) == 0 {
			abandoned := p.mutation + "." + o.Token + ".abandoned"
			if os.Rename(p.mutation, abandoned) == nil {
				var moved mutationOwner
				if readJSON(filepath.Join(abandoned, "owner.json"), &moved) && moved.Token == o.Token {
					os.RemoveAll(abandoned)
					continue
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("Timed out serializing LocalBase operation ownership for %s.", p.lock)
}

// removeAbandonedLocked removes the lease of operationID when its owner is
// gone (or its handoff expired). Call it holding the mutation lock.
func removeAbandonedLocked(p opPaths, operationID string) (bool, error) {
	var o operationOwner
	if !readJSON(p.owner, &o) || !o.valid() || o.OperationID != operationID {
		return false, nil
	}
	if o.HandoffExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, o.HandoffExpiresAt); err == nil && t.After(time.Now()) {
			return false, nil
		}
	} else if presence(o.PID) != 0 {
		return false, nil
	}
	abandoned := p.lock + "." + operationID + ".abandoned"
	if err := os.Rename(p.lock, abandoned); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var moved operationOwner
	if !readJSON(filepath.Join(abandoned, "owner.json"), &moved) || moved.OperationID != operationID {
		return false, fmt.Errorf("Operation ownership changed while reclaiming %s.", p.lock)
	}
	os.RemoveAll(abandoned)
	return true, nil
}

// Lease is a held root operation.
type Lease struct {
	id        string
	paths     opPaths
	handedOff bool
	claimed   bool
}

// AcquireOperation takes the operation lease of a canonical root, waiting up
// to 10 s for another operation to finish.
func AcquireOperation(canonicalRoot, operation string) (*Lease, error) {
	p := operationPaths(canonicalRoot)
	if err := ensurePrivateDir(p.dir); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	var invalidSince time.Time
	for time.Now().Before(deadline) {
		err := os.Mkdir(p.lock, 0o700)
		if err == nil {
			owner := operationOwner{Version: 1, OperationID: uuid(), Operation: operation, Root: canonicalRoot,
				RootHash: Hash(canonicalRoot), PID: os.Getpid(), StartedAt: nowISO()}
			if err := writeJSONAtomic(p.owner, owner); err != nil {
				os.RemoveAll(p.lock)
				return nil, err
			}
			return &Lease{id: owner.OperationID, paths: p}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		reclaimed := false
		if err := withMutation(p, func() error {
			var o operationOwner
			if readJSON(p.owner, &o) && o.valid() && o.Root == canonicalRoot && o.RootHash == Hash(canonicalRoot) {
				var err error
				reclaimed, err = removeAbandonedLocked(p, o.OperationID)
				return err
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if reclaimed {
			continue
		}
		var o operationOwner
		ok := readJSON(p.owner, &o) && o.valid()
		if !ok {
			if _, err := os.Stat(p.owner); errors.Is(err, os.ErrNotExist) {
				invalidSince = time.Time{}
				time.Sleep(25 * time.Millisecond)
				continue
			}
			if invalidSince.IsZero() {
				invalidSince = time.Now()
			}
			if time.Since(invalidSince) >= 250*time.Millisecond {
				return nil, fmt.Errorf("Cannot coordinate LocalBase operations for %s: operation metadata is invalid.", canonicalRoot)
			}
			time.Sleep(25 * time.Millisecond)
			continue
		}
		invalidSince = time.Time{}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("Timed out waiting to %s LocalBase root %s; another LocalBase operation is still active or its owner cannot be verified.",
		operation, canonicalRoot)
}

// Handoff passes the lease to the gateway a service start launches: the
// gateway holding serviceToken claims it within 10 s.
func (l *Lease) Handoff(serviceToken string) error {
	if l.claimed {
		return fmt.Errorf("Cannot hand off claimed LocalBase operation ownership.")
	}
	return withMutation(l.paths, func() error {
		var o operationOwner
		if !readJSON(l.paths.owner, &o) || !o.valid() || o.OperationID != l.id || o.HandoffToken != "" {
			return fmt.Errorf("Cannot hand off LocalBase operation ownership for %s.", filepath.Base(l.paths.lock))
		}
		o.Operation = "service-start-handoff"
		o.HandoffToken = serviceToken
		o.HandoffExpiresAt = time.Now().Add(handoffTTL).UTC().Format("2006-01-02T15:04:05.000Z")
		if err := writeJSONAtomic(l.paths.owner, o); err != nil {
			return err
		}
		l.handedOff = true
		return nil
	})
}

// Release ends the lease unless it was handed off.
func (l *Lease) Release() {
	if l == nil || l.handedOff {
		return
	}
	withMutation(l.paths, func() error {
		var o operationOwner
		if readJSON(l.paths.owner, &o) && o.OperationID == l.id {
			os.RemoveAll(l.paths.lock)
		}
		return nil
	})
}

// WithOperation runs fn holding the root's operation lease.
func WithOperation(canonicalRoot, operation string, fn func() error) error {
	l, err := AcquireOperation(canonicalRoot, operation)
	if err != nil {
		return err
	}
	defer l.Release()
	return fn()
}

// ServeInitialization is the lease a starting gateway holds until it owns
// the gateway lock. A service-launched gateway (serviceToken set) claims the
// lease its service start handed off.
func ServeInitialization(canonicalRoot, serviceToken string) (*Lease, error) {
	if serviceToken == "" {
		return AcquireOperation(canonicalRoot, "serve-initialize")
	}
	p := operationPaths(canonicalRoot)
	deadline := time.Now().Add(handoffTTL)
	for time.Now().Before(deadline) {
		state, lease, err := claimHandoff(p, canonicalRoot, serviceToken)
		if err != nil {
			return nil, err
		}
		switch state {
		case "claimed":
			return lease, nil
		case "missing":
			return AcquireOperation(canonicalRoot, "serve-initialize")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil, fmt.Errorf("Timed out waiting for LocalBase service start handoff for %s.", canonicalRoot)
}

func claimHandoff(p opPaths, canonicalRoot, serviceToken string) (state string, lease *Lease, err error) {
	if !isDir(p.lock) {
		return "missing", nil, nil
	}
	err = withMutation(p, func() error {
		state = "waiting"
		if !isDir(p.lock) {
			state = "missing"
			return nil
		}
		var o operationOwner
		if !readJSON(p.owner, &o) || !o.valid() || o.Root != canonicalRoot || o.RootHash != Hash(canonicalRoot) ||
			o.HandoffToken == "" {
			return nil
		}
		if t, err := time.Parse(time.RFC3339Nano, o.HandoffExpiresAt); err != nil || !t.After(time.Now()) {
			if _, err := removeAbandonedLocked(p, o.OperationID); err != nil {
				return err
			}
			state = "missing"
			return nil
		}
		if o.HandoffToken != serviceToken {
			return nil
		}
		claimed := operationOwner{Version: 1, OperationID: uuid(), Operation: "serve-initialize", Root: canonicalRoot,
			RootHash: Hash(canonicalRoot), PID: os.Getpid(), StartedAt: nowISO()}
		if err := writeJSONAtomic(p.owner, claimed); err != nil {
			return err
		}
		state, lease = "claimed", &Lease{id: claimed.OperationID, paths: p, claimed: true}
		return nil
	})
	return state, lease, err
}
