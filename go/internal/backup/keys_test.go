package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// keyRoot is a root with one key, and a backup made while that key was as it started.
type keyRoot struct {
	r            *root.Root
	file, secret string
	id           string
}

func withKey(t *testing.T) *keyRoot {
	t.Helper()
	r, secret := newRoot(t, dir(t))
	k := &keyRoot{r: r, secret: secret, file: create(t, r, false).Output}
	k.onRoot(t, func(st *store.Store) {
		found, err := st.ResolveAPIKey(context.Background(), secret)
		if err != nil || found == nil {
			t.Fatalf("lookup: %v", err)
		}
		k.id = found.ID
	})
	return k
}

// onRoot changes the root the way the gateway would, and closes the database before a restore.
func (k *keyRoot) onRoot(t *testing.T, f func(*store.Store)) {
	t.Helper()
	st, err := store.Open(k.r.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f(st)
}

func (k *keyRoot) rotate(t *testing.T) (current string) {
	t.Helper()
	k.onRoot(t, func(st *store.Store) {
		var err error
		if _, current, err = st.RotateAPIKey(context.Background(), k.id); err != nil {
			t.Fatal(err)
		}
	})
	return current
}

func (k *keyRoot) works(t *testing.T, secret string) bool {
	t.Helper()
	var found bool
	k.onRoot(t, func(st *store.Store) {
		got, err := st.ResolveAPIKey(context.Background(), secret)
		if err != nil {
			t.Fatal(err)
		}
		found = got != nil
	})
	return found
}

// row is every column that decides what a key is, as text, so that nothing can change unseen.
func (k *keyRoot) row(t *testing.T, id string) string {
	t.Helper()
	var got string
	k.onRoot(t, func(st *store.Store) {
		err := st.DB.QueryRow(`SELECT quote(name)||quote(prefix)||quote(key_hash)||quote(scopes)||quote(last_rotated_at)||quote(expires_at)||quote(revoked_at)
			FROM api_keys WHERE id = ?`, id).Scan(&got)
		if err != nil {
			t.Fatalf("row %s: %v", id, err)
		}
	})
	return got
}

// restoreNaming restores over the root, taking the backup's version of the keys named.
func restoreNaming(t *testing.T, file, target string, ids ...string) *Report {
	t.Helper()
	s, err := Stage(context.Background(), file, target, true, ids...)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	rep := s.Report
	if _, err := s.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return rep
}

func oneKey(t *testing.T, what string, got []KeyChange, want KeyChange) {
	t.Helper()
	if len(got) != 1 || got[0] != want {
		t.Errorf("%s: %+v, want just %+v", what, got, want)
	}
}

func noKeys(t *testing.T, rep *Report, except ...string) {
	t.Helper()
	for name, list := range map[string][]KeyChange{"kept": rep.KeysKept, "left out": rep.KeysLeftOut, "brought back": rep.KeysBroughtBack, "lost": rep.KeysLost, "active": rep.KeysActive} {
		skip := false
		for _, e := range except {
			skip = skip || e == name
		}
		if !skip && len(list) != 0 {
			t.Errorf("%s: %+v", name, list)
		}
	}
}

// A key is rotated because its secret may have got out, so an older backup can't bring that secret
// back. The current one goes on working.
func TestARotatedKeyStaysRotated(t *testing.T) {
	k := withKey(t)
	current := k.rotate(t)
	before := k.row(t, k.id)
	rep, _ := restore(t, k.file, k.r.Dir, true)
	if k.works(t, k.secret) {
		t.Error("the old secret works")
	}
	if !k.works(t, current) {
		t.Error("the current secret doesn't work")
	}
	if got := k.row(t, k.id); got != before {
		t.Errorf("the key changed:\n%s\n%s", before, got)
	}
	oneKey(t, "kept", rep.KeysKept, KeyChange{k.id, "kept", "rotated"})
	noKeys(t, rep, "kept")
}

// Named, the backup's version comes back whole, and the report says which.
func TestARotatedKeyComesBackOnlyByName(t *testing.T) {
	k := withKey(t)
	current := k.rotate(t)
	rep := restoreNaming(t, k.file, k.r.Dir, k.id)
	if !k.works(t, k.secret) {
		t.Error("the backup's secret doesn't work")
	}
	if k.works(t, current) {
		t.Error("the secret it had still works")
	}
	oneKey(t, "brought back", rep.KeysBroughtBack, KeyChange{k.id, "kept", "rotated"})
	noKeys(t, rep, "brought back")
}

// Naming one key leaves the others to the rule.
func TestNamingOneKeyLeavesTheOthers(t *testing.T) {
	k := withKey(t)
	var otherID, otherOld, otherNow string
	k.onRoot(t, func(st *store.Store) {
		key, secret, err := st.CreateAPIKey(context.Background(), "other", []string{"inference:chat"})
		if err != nil {
			t.Fatal(err)
		}
		otherID, otherOld = key.ID, secret
	})
	file := create(t, k.r, false).Output
	k.rotate(t)
	k.onRoot(t, func(st *store.Store) {
		var err error
		if _, otherNow, err = st.RotateAPIKey(context.Background(), otherID); err != nil {
			t.Fatal(err)
		}
	})
	rep := restoreNaming(t, file, k.r.Dir, k.id)
	if !k.works(t, k.secret) {
		t.Error("the one named didn't come back")
	}
	if k.works(t, otherOld) || !k.works(t, otherNow) {
		t.Errorf("the one not named: old %v, now %v", k.works(t, otherOld), k.works(t, otherNow))
	}
	oneKey(t, "brought back", rep.KeysBroughtBack, KeyChange{k.id, "kept", "rotated"})
	oneKey(t, "kept", rep.KeysKept, KeyChange{otherID, "other", "rotated"})
}

// A file with any one thing about a key different, whether from an older state or written by hand:
// what the root has wins, in every column, and the report says what differed.
func TestEachThingAboutAKeyIsCompared(t *testing.T) {
	for _, c := range []struct {
		column, value, why string
	}{
		{"key_hash", "'another hash'", "rotated"},
		{"prefix", "'lb_other'", "rotated"},
		{"last_rotated_at", "'2001-01-01T00:00:00.000Z'", "rotated"},
		{"scopes", `'["keys:manage"]'`, "permissions changed"},
		{"expires_at", "'2099-01-01T00:00:00.000Z'", "expiry changed"},
		{"name", "'another name'", "renamed"},
		{"revoked_at", "'2001-01-01T00:00:00.000Z'", "revoked in this backup and not on this root"},
	} {
		t.Run(c.column, func(t *testing.T) {
			k := withKey(t)
			bad := editedBackup(t, k.file, `UPDATE api_keys SET `+c.column+` = `+c.value+` WHERE id = '`+k.id+`'`)
			before := k.row(t, k.id)
			rep, _ := restore(t, bad, k.r.Dir, true)
			if got := k.row(t, k.id); got != before {
				t.Errorf("the key changed:\n%s\n%s", before, got)
			}
			if !k.works(t, k.secret) {
				t.Error("its secret stopped working")
			}
			oneKey(t, "kept", rep.KeysKept, KeyChange{k.id, "kept", c.why})
			if rep.RevocationsKept != 0 {
				t.Errorf("revocations kept: %d", rep.RevocationsKept)
			}
		})
	}
}

// addAdminKey puts a key in a backup that the root has never had, one that can run the root.
func addAdminKey(t *testing.T, file, scopes string) (bad string) {
	t.Helper()
	return editedBackup(t, file, `INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, scopes)
		VALUES ('key_admin', 'admin', 'lb_admin', '`+store.HashKey(adminSecret)+`', 'x', 'x', '`+scopes+`')`)
}

const adminSecret = "lb_a-secret-for-the-key-a-file-planted"

// Where nothing could show a key was never there, one that can run the root stays out and an ordinary
// one comes: on a root with no keys, on a new root, and on one that can't be read.
func TestAnAdminKeyIsHeldBackWhereThereIsNothingToCompare(t *testing.T) {
	for name, target := range map[string]func(*testing.T, *keyRoot) (string, bool){
		"a root with no keys": func(t *testing.T, k *keyRoot) (string, bool) {
			k.onRoot(t, func(st *store.Store) {
				if err := st.DeleteAPIKey(context.Background(), k.id); err != nil {
					t.Fatal(err)
				}
			})
			return k.r.Dir, true
		},
		"a new root": func(t *testing.T, k *keyRoot) (string, bool) {
			return filepath.Join(dir(t), "target"), false
		},
		"a root that can't be read": func(t *testing.T, k *keyRoot) (string, bool) {
			if err := os.WriteFile(k.r.DB(), []byte(strings.Repeat("not a database ", 400)), 0o600); err != nil {
				t.Fatal(err)
			}
			return k.r.Dir, true
		},
	} {
		t.Run(name, func(t *testing.T) {
			k := withKey(t)
			bad := addAdminKey(t, k.file, `["inference:chat","keys:manage","access:manage"]`)
			where, replace := target(t, k)

			s, err := Stage(context.Background(), bad, where, replace)
			if err != nil {
				t.Fatalf("stage: %v", err)
			}
			rep := s.Report
			if _, err := s.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			oneKey(t, "held back", rep.KeysHeldBack, KeyChange{"key_admin", "admin", "access:manage, keys:manage"})
			oneKey(t, "active", rep.KeysActive, KeyChange{k.id, "kept", "inference:chat"})
			restored := &keyRoot{r: &root.Root{Dir: where}}
			if restored.works(t, adminSecret) {
				t.Error("the key that can run the root works")
			}
			if !restored.works(t, k.secret) {
				t.Error("the ordinary key doesn't work")
			}
		})
		// Named, it comes in each of these, since a broken root is just when a backup is needed.
		t.Run(name+" naming it", func(t *testing.T) {
			k := withKey(t)
			bad := addAdminKey(t, k.file, `["inference:chat","keys:manage","access:manage"]`)
			where, replace := target(t, k)

			s, err := Stage(context.Background(), bad, where, replace, "key_admin")
			if err != nil {
				t.Fatalf("stage: %v", err)
			}
			rep := s.Report
			if _, err := s.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			oneKey(t, "brought back", rep.KeysBroughtBack, KeyChange{"key_admin", "admin", "can do access:manage, keys:manage"})
			if len(rep.KeysHeldBack) != 0 {
				t.Errorf("held back: %+v", rep.KeysHeldBack)
			}
			restored := &keyRoot{r: &root.Root{Dir: where}}
			if !restored.works(t, adminSecret) {
				t.Error("the key that was named doesn't work")
			}
			if !restored.works(t, k.secret) {
				t.Error("the ordinary key doesn't work")
			}
		})
	}
}

// Named, it comes, and the report says what it can do.
func TestAnAdminKeyComesBackByName(t *testing.T) {
	k := withKey(t)
	bad := addAdminKey(t, k.file, `["keys:manage"]`)
	k.onRoot(t, func(st *store.Store) {
		if err := st.DeleteAPIKey(context.Background(), k.id); err != nil {
			t.Fatal(err)
		}
	})
	rep := restoreNaming(t, bad, k.r.Dir, "key_admin")
	if !k.works(t, adminSecret) {
		t.Error("the named key doesn't work")
	}
	if !k.works(t, k.secret) {
		t.Error("the ordinary key doesn't work")
	}
	oneKey(t, "brought back", rep.KeysBroughtBack, KeyChange{"key_admin", "admin", "can do keys:manage"})
	if len(rep.KeysHeldBack) != 0 {
		t.Errorf("held back: %+v", rep.KeysHeldBack)
	}
	active := map[string]bool{}
	for _, a := range rep.KeysActive {
		active[a.ID] = true
	}
	if !active["key_admin"] || !active[k.id] {
		t.Errorf("active: %+v", rep.KeysActive)
	}
}

// What the code gives a new key is ordinary. Everything else is held back, including a permission only
// a file has heard of.
func TestEveryPermissionBeyondMakingRequestsIsHeldBack(t *testing.T) {
	for _, scope := range append(append([]string{}, auth.Permissions...), "something:new", "*") {
		k := comparedKey{scopes: sql.NullString{String: `["` + scope + `"]`, Valid: true}}
		got := k.beyondRequests()
		if slices.Contains(auth.DefaultKeyScopes, scope) {
			if len(got) != 0 {
				t.Errorf("%s counts as more than making requests: %v", scope, got)
			}
			continue
		}
		if len(got) != 1 || got[0] != scope {
			t.Errorf("%s: %v", scope, got)
		}
	}
	// One through a real restore, for each side of that line. The rest of it is tested elsewhere with
	// other permissions: see TestAnAdminKeyIsHeldBackWhereThereIsNothingToCompare.
	k := withKey(t)
	for _, c := range []struct {
		scope string
		held  bool
	}{{"something:new", true}, {auth.DefaultKeyScopes[0], false}} {
		rep := stageReport(t, addAdminKey(t, k.file, `["`+c.scope+`"]`), filepath.Join(dir(t), "target"+c.scope), false)
		if c.held {
			oneKey(t, "held back", rep.KeysHeldBack, KeyChange{"key_admin", "admin", c.scope})
		} else if len(rep.KeysHeldBack) != 0 {
			t.Errorf("held back: %+v", rep.KeysHeldBack)
		}
	}
}

// A key held back isn't lost for as long as there's the backup: the same restore again, naming it. Both
// ways a root can be by then: with the ordinary keys the first restore brought, and with none.
func TestAHeldBackKeyCanComeBackAfterTheRestore(t *testing.T) {
	t.Run("with other keys there by then", func(t *testing.T) {
		k := withKey(t)
		bad := addAdminKey(t, k.file, `["keys:manage"]`)
		target := filepath.Join(dir(t), "target")
		s, err := Stage(context.Background(), bad, target, false)
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		if _, err := s.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		restored := &keyRoot{r: &root.Root{Dir: target}}
		if restored.works(t, adminSecret) {
			t.Fatal("it wasn't held back")
		}
		rep := restoreNaming(t, bad, target, "key_admin")
		if !restored.works(t, adminSecret) {
			t.Error("it didn't come back")
		}
		if !restored.works(t, k.secret) {
			t.Error("the ordinary key stopped working")
		}
		oneKey(t, "brought back", rep.KeysBroughtBack, KeyChange{"key_admin", "admin", "this root doesn't have it"})
	})
	t.Run("with no keys there", func(t *testing.T) {
		k := withKey(t)
		bad := editedBackup(t, k.file, `DELETE FROM api_keys`, `INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, scopes)
			VALUES ('key_admin', 'admin', 'lb_admin', '`+store.HashKey(adminSecret)+`', 'x', 'x', '["keys:manage"]')`)
		target := filepath.Join(dir(t), "target")
		s, err := Stage(context.Background(), bad, target, false)
		if err != nil {
			t.Fatalf("stage: %v", err)
		}
		if _, err := s.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		restored := &keyRoot{r: &root.Root{Dir: target}}
		rep := restoreNaming(t, bad, target, "key_admin")
		if !restored.works(t, adminSecret) {
			t.Error("it didn't come back")
		}
		oneKey(t, "brought back", rep.KeysBroughtBack, KeyChange{"key_admin", "admin", "can do keys:manage"})
	})
}

// When the root can't be read, what changes isn't known, so that's what's said, for roles and for who
// can sign in.
func TestAnUnreadableRootSaysItMayChangeThings(t *testing.T) {
	rootDir, file := withRole(t)
	if err := os.WriteFile(filepath.Join(rootDir, "local-base.db"), []byte(strings.Repeat("not a database ", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := stageReport(t, file, rootDir, true)
	if !caution(rep, grantsUnknownCaution) || caution(rep, grantsChangedCaution) {
		t.Errorf("roles: %v", rep.Cautions)
	}
	if !caution(rep, signInUnknownCaution) || caution(rep, signInChangedCaution) {
		t.Errorf("sign-in: %v", rep.Cautions)
	}
}

// What can be read and differs is still said to change, whatever else can't be read.
func TestAKnownChangeIsSaidToChangeThings(t *testing.T) {
	s, caution := signInReport(signInValues{mail: "new.example:25"}, &signInValues{mail: "old.example:25", accessErr: context.Canceled})
	if caution != signInChangedCaution {
		t.Errorf("%q, %+v", caution, s)
	}
}

// A list of permissions that doesn't read can't be taken for an ordinary key. A file holding one is
// refused before this, so this is only the rule, with the rule's own reason.
func TestPermissionsThatDoNotReadAreMoreThanMakingRequests(t *testing.T) {
	k := comparedKey{scopes: sql.NullString{String: "not a list", Valid: true}}
	if got := k.beyondRequests(); len(got) != 1 || got[0] != "not a list" {
		t.Errorf("%q", got)
	}
}

func TestKeysHeldBackAreInOrder(t *testing.T) {
	k := withKey(t)
	var planted []string
	for _, l := range strings.Split("f,c,a,e,b,d", ",") {
		planted = append(planted, `INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, scopes)
			VALUES ('key_`+l+`', 'n', 'p', 'h`+l+`', 'x', 'x', '["keys:manage"]')`)
	}
	bad := editedBackup(t, k.file, planted...)
	rep := stageReport(t, bad, filepath.Join(dir(t), "target"), false)
	var ids []string
	for _, h := range rep.KeysHeldBack {
		ids = append(ids, h.ID)
	}
	if strings.Join(ids, ",") != "key_a,key_b,key_c,key_d,key_e,key_f" {
		t.Errorf("%v", ids)
	}
}

// A revoked key does nothing, so nothing is said about it, and it stays revoked.
func TestARevokedAdminKeyIsNotHeldBack(t *testing.T) {
	k := withKey(t)
	bad := editedBackup(t, k.file, `INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, revoked_at, scopes)
		VALUES ('key_admin', 'admin', 'lb_admin', '`+store.HashKey(adminSecret)+`', 'x', 'x', 'x', '["keys:manage"]')`)
	target := filepath.Join(dir(t), "target")
	s, err := Stage(context.Background(), bad, target, false)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	rep := s.Report
	if _, err := s.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(rep.KeysHeldBack) != 0 {
		t.Errorf("held back: %+v", rep.KeysHeldBack)
	}
	restored := &keyRoot{r: &root.Root{Dir: target}}
	if restored.works(t, adminSecret) {
		t.Error("the revoked key works")
	}
}

// A root with keys of its own has this covered already: the key stays out unless it's named.
func TestAnAdminKeyStaysOutOfARootWithKeys(t *testing.T) {
	k := withKey(t)
	bad := addAdminKey(t, k.file, `["keys:manage"]`)
	rep, _ := restore(t, bad, k.r.Dir, true)
	oneKey(t, "left out", rep.KeysLeftOut, KeyChange{"key_admin", "admin", "keys:manage"})
	noKeys(t, rep, "left out")
	if k.works(t, adminSecret) {
		t.Error("the key that can run the root works")
	}
	rep = restoreNaming(t, bad, k.r.Dir, "key_admin")
	if !k.works(t, adminSecret) {
		t.Error("the named key doesn't work")
	}
	oneKey(t, "brought back", rep.KeysBroughtBack, KeyChange{"key_admin", "admin", "this root doesn't have it"})
}

// A root too old to say what its key may do can't vouch for a file saying it may run the root.
func TestARootOlderThanKeyPermissionsCannotVouchForAnAdminKey(t *testing.T) {
	k := withKey(t)
	bad := editedBackup(t, k.file, `UPDATE api_keys SET scopes = '["keys:manage"]' WHERE id = '`+k.id+`'`)
	k.onRoot(t, func(st *store.Store) {
		if _, err := st.DB.Exec(`ALTER TABLE api_keys DROP COLUMN scopes`); err != nil {
			t.Fatal(err)
		}
	})
	s, err := Stage(context.Background(), bad, k.r.Dir, true)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	rep := s.Report
	s.Discard()
	oneKey(t, "held back", rep.KeysHeldBack, KeyChange{k.id, "kept", "keys:manage"})
	if len(rep.KeysActive) != 0 {
		t.Errorf("active: %+v", rep.KeysActive)
	}
	named, err := Stage(context.Background(), bad, k.r.Dir, true, k.id)
	if err != nil {
		t.Fatalf("stage naming it: %v", err)
	}
	defer named.Discard()
	oneKey(t, "brought back", named.Report.KeysBroughtBack, KeyChange{k.id, "kept", "can do keys:manage"})

	// Revoked in the file, it does nothing, so there's nothing to hold back.
	revoked := editedBackup(t, k.file, `UPDATE api_keys SET scopes = '["keys:manage"]', revoked_at = 'x' WHERE id = '`+k.id+`'`)
	rep = stageReport(t, revoked, k.r.Dir, true)
	if len(rep.KeysHeldBack) != 0 {
		t.Errorf("a revoked key held back: %+v", rep.KeysHeldBack)
	}
}

// Everything that differs is named, and comparing is by what's stored, not by a date alone.
func TestEverythingThatDiffersIsSaid(t *testing.T) {
	k := withKey(t)
	bad := editedBackup(t, k.file, `UPDATE api_keys SET key_hash = 'x', scopes = '["keys:manage"]', name = 'y' WHERE id = '`+k.id+`'`)
	rep := stageReport(t, bad, k.r.Dir, true)
	oneKey(t, "kept", rep.KeysKept, KeyChange{k.id, "kept", "rotated, permissions changed, renamed"})
}

// A key narrowed on this root doesn't get back what it could do.
func TestNarrowedPermissionsStayNarrow(t *testing.T) {
	ctx := context.Background()
	r, secret := newRoot(t, dir(t))
	var id string
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	strong, strongSecret, err := st.CreateAPIKey(ctx, "strong", []string{"inference:chat", "keys:manage"})
	if err != nil {
		t.Fatal(err)
	}
	id = strong.ID
	st.Close()
	file := create(t, r, false).Output
	k := &keyRoot{r: r, file: file, secret: secret}
	k.onRoot(t, func(st *store.Store) {
		if _, err := st.SetAPIKeyScopes(ctx, id, []string{"inference:chat"}); err != nil {
			t.Fatal(err)
		}
	})
	rep, _ := restore(t, file, r.Dir, true)
	k.onRoot(t, func(st *store.Store) {
		got, err := st.ResolveAPIKey(ctx, strongSecret)
		if err != nil || got == nil {
			t.Fatalf("lookup: %v", err)
		}
		if strings.Join(got.Scopes, ",") != "inference:chat" {
			t.Errorf("it can do: %v", got.Scopes)
		}
	})
	oneKey(t, "kept", rep.KeysKept, KeyChange{id, "strong", "permissions changed"})
}

// Revoked stays revoked unless it's named, and it's said either way.
func TestARevokedKeyComesBackOnlyByName(t *testing.T) {
	k := withKey(t)
	k.onRoot(t, func(st *store.Store) {
		if _, err := st.RevokeAPIKey(context.Background(), k.id); err != nil {
			t.Fatal(err)
		}
	})
	rep, _ := restore(t, k.file, k.r.Dir, true)
	if k.works(t, k.secret) {
		t.Error("the revoked key works")
	}
	if rep.RevocationsKept != 1 {
		t.Errorf("revocations kept: %d", rep.RevocationsKept)
	}
	oneKey(t, "kept", rep.KeysKept, KeyChange{k.id, "kept", "revoked"})
	rep = restoreNaming(t, k.file, k.r.Dir, k.id)
	if !k.works(t, k.secret) {
		t.Error("the named key doesn't work")
	}
	if rep.RevocationsKept != 0 {
		t.Errorf("revocations kept when named: %d", rep.RevocationsKept)
	}
	oneKey(t, "brought back", rep.KeysBroughtBack, KeyChange{k.id, "kept", "revoked on this root, and would work again"})
}

// Going back, a key revoked on this root says it would work again, before anything else that differs.
// Staying, it keeps "revoked", which is what stays true there.
func TestARevokedKeyGoingBackSaysItWouldWorkAgain(t *testing.T) {
	k := withKey(t)
	k.onRoot(t, func(st *store.Store) {
		if _, err := st.RevokeAPIKey(context.Background(), k.id); err != nil {
			t.Fatal(err)
		}
	})
	rotated := editedBackup(t, k.file, `UPDATE api_keys SET key_hash = 'x' WHERE id = '`+k.id+`'`)
	kept := stageReport(t, rotated, k.r.Dir, true)
	oneKey(t, "kept", kept.KeysKept, KeyChange{k.id, "kept", "revoked, rotated"})
	s, err := Stage(context.Background(), rotated, k.r.Dir, true, k.id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Discard()
	oneKey(t, "brought back", s.Report.KeysBroughtBack, KeyChange{k.id, "kept", "revoked on this root, and would work again, rotated"})
}

// Revoked in both is nothing to say, whatever else differs about when.
func TestRevokedInBothSaysNothing(t *testing.T) {
	k := withKey(t)
	k.onRoot(t, func(st *store.Store) {
		if _, err := st.RevokeAPIKey(context.Background(), k.id); err != nil {
			t.Fatal(err)
		}
	})
	file := create(t, k.r, false).Output
	bad := editedBackup(t, file, `UPDATE api_keys SET revoked_at = '2001-01-01T00:00:00.000Z' WHERE id = '`+k.id+`'`)
	rep := stageReport(t, bad, k.r.Dir, true)
	noKeys(t, rep)
}

// Removed from this root, or never on it: a backup doesn't get to add a key, unless it's named.
func TestAKeyThisRootDoesNotHaveStaysOut(t *testing.T) {
	k := withKey(t)
	var otherID, otherSecret string
	k.onRoot(t, func(st *store.Store) {
		key, secret, err := st.CreateAPIKey(context.Background(), "made since", []string{"inference:chat"})
		if err != nil {
			t.Fatal(err)
		}
		otherID, otherSecret = key.ID, secret
		if err := st.DeleteAPIKey(context.Background(), k.id); err != nil {
			t.Fatal(err)
		}
	})
	rep := stageReport(t, k.file, k.r.Dir, true)
	oneKey(t, "left out", rep.KeysLeftOut, KeyChange{k.id, "kept", "inference:chat"})
	oneKey(t, "lost", rep.KeysLost, KeyChange{ID: otherID, Name: "made since"})
	noKeys(t, rep, "left out", "lost")

	restore(t, k.file, k.r.Dir, true)
	if k.works(t, k.secret) {
		t.Error("the key this root doesn't have came back")
	}
	if k.works(t, otherSecret) {
		t.Error("the key the backup doesn't hold still works")
	}
}

// A key already revoked on this root does nothing, so its going isn't worth saying. The list is of keys
// that would stop working.
func TestARevokedKeyTheBackupDoesNotHoldIsNotLost(t *testing.T) {
	k := withKey(t)
	k.onRoot(t, func(st *store.Store) {
		ctx := context.Background()
		gone, _, err := st.CreateAPIKey(ctx, "revoked since", []string{"inference:chat"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.RevokeAPIKey(ctx, gone.ID); err != nil {
			t.Fatal(err)
		}
	})
	rep := stageReport(t, k.file, k.r.Dir, true)
	noKeys(t, rep)
}

func TestAKeyThisRootDoesNotHaveComesBackByName(t *testing.T) {
	k := withKey(t)
	k.onRoot(t, func(st *store.Store) {
		if _, _, err := st.CreateAPIKey(context.Background(), "made since", []string{"inference:chat"}); err != nil {
			t.Fatal(err)
		}
		if err := st.DeleteAPIKey(context.Background(), k.id); err != nil {
			t.Fatal(err)
		}
	})
	rep := restoreNaming(t, k.file, k.r.Dir, k.id)
	if !k.works(t, k.secret) {
		t.Error("the named key isn't back")
	}
	oneKey(t, "brought back", rep.KeysBroughtBack, KeyChange{k.id, "kept", "this root doesn't have it"})
	if len(rep.KeysLeftOut) != 0 {
		t.Errorf("left out: %+v", rep.KeysLeftOut)
	}
}

// A file adding a key to a root that has its own: it's the same case, and it stays out.
func TestAKeyAddedToAFileStaysOut(t *testing.T) {
	k := withKey(t)
	bad := editedBackup(t, k.file, `INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, scopes)
		VALUES ('key_added', 'added', 'lb_added', 'added hash', 'x', 'x', '["keys:manage"]')`)
	rep, _ := restore(t, bad, k.r.Dir, true)
	oneKey(t, "left out", rep.KeysLeftOut, KeyChange{"key_added", "added", "keys:manage"})
	k.onRoot(t, func(st *store.Store) {
		var n int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE id = 'key_added'`).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d rows, %v", n, err)
		}
	})
}

// A root with no keys at all has nothing to set a backup's against. That's how a backup goes back
// onto a fresh install, so its keys come, and they're listed since they weren't compared.
func TestARootWithNoKeysTakesTheBackups(t *testing.T) {
	k := withKey(t)
	k.onRoot(t, func(st *store.Store) {
		if err := st.DeleteAPIKey(context.Background(), k.id); err != nil {
			t.Fatal(err)
		}
	})
	rep, _ := restore(t, k.file, k.r.Dir, true)
	if !k.works(t, k.secret) {
		t.Error("the backup's key doesn't work")
	}
	oneKey(t, "active", rep.KeysActive, KeyChange{k.id, "kept", "inference:chat"})
	noKeys(t, rep, "active")
}

// On a new root there's nothing to compare either, so what would work is listed with what it can do.
func TestKeysOnANewRootAreListedWithWhatTheyCanDo(t *testing.T) {
	k := withKey(t)
	bad := editedBackup(t, k.file,
		`INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, scopes)
			VALUES ('key_a', 'all of it', 'lb_a', 'all hash', 'x', 'x', '["inference:chat","inference:embeddings","models:read"]')`,
		`INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, scopes)
			VALUES ('key_b_empty', 'empty', 'lb_e', 'empty hash', 'x', 'x', '[]')`,
		`INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, revoked_at, scopes)
			VALUES ('key_c_revoked', 'revoked', 'lb_r', 'revoked hash', 'x', 'x', 'x', '["inference:chat"]')`,
		`INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, expires_at, scopes)
			VALUES ('key_d_expired', 'expired', 'lb_x', 'expired hash', 'x', 'x', '2001-01-01T00:00:00.000Z', '["inference:chat"]')`)
	rep := stageReport(t, bad, filepath.Join(dir(t), "target"), false)
	want := map[string]string{"key_a": "inference:chat, inference:embeddings, models:read", "key_b_empty": "no permissions", k.id: "inference:chat"}
	if len(rep.KeysActive) != len(want) {
		t.Fatalf("%+v", rep.KeysActive)
	}
	for _, got := range rep.KeysActive {
		if want[got.ID] != got.Why {
			t.Errorf("%+v, want %q", got, want[got.ID])
		}
	}
	noKeys(t, rep, "active")
	if !caution(rep, "Any key revoked or rotated since this backup was made works again here. Check them afterwards: local-base keys list") {
		t.Errorf("cautions: %v", rep.Cautions)
	}
}

// Nothing changed, so nothing about keys is said at all.
func TestTheSameKeysSayNothing(t *testing.T) {
	k := withKey(t)
	rep := stageReport(t, k.file, k.r.Dir, true)
	noKeys(t, rep)
	if rep.RevocationsKept != 0 {
		t.Errorf("revocations kept: %d", rep.RevocationsKept)
	}
}

// Naming something that wouldn't change anything is a mistake worth stopping for, since it could be
// a mistyped id with the real one left on its current secret.
func TestNamingAKeyThatChangesNothingIsRefused(t *testing.T) {
	k := withKey(t)
	revoked := editedBackup(t, k.file, `INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, revoked_at, scopes)
		VALUES ('key_revoked', 'revoked', 'lb_r', 'revoked hash', 'x', 'x', 'x', '[]')`)
	for name, c := range map[string]struct{ file, id string }{
		"the same key":                  {k.file, k.id},
		"no such key":                   {k.file, "key_nothing"},
		"a key revoked and not on root": {revoked, "key_revoked"},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Stage(context.Background(), c.file, k.r.Dir, true, c.id)
			var refused *Refused
			if !errors.As(err, &refused) {
				if s != nil {
					s.Discard()
				}
				t.Fatalf("not refused: %v", err)
			}
			if !strings.Contains(err.Error(), "--bring-back-key "+c.id+": there's nothing to bring back.") {
				t.Errorf("%v", err)
			}
		})
	}
}

// With a real id and a mistyped one, the mistake still stops it, and nothing has been done.
func TestAMistypedNameStopsTheRest(t *testing.T) {
	k := withKey(t)
	current := k.rotate(t)
	s, err := Stage(context.Background(), k.file, k.r.Dir, true, k.id, "key_nothing")
	var refused *Refused
	if !errors.As(err, &refused) {
		if s != nil {
			s.Discard()
		}
		t.Fatalf("not refused: %v", err)
	}
	if !k.works(t, current) {
		t.Error("the root changed")
	}
}

func TestNamingAKeyOnANewRootIsRefused(t *testing.T) {
	k := withKey(t)
	s, err := Stage(context.Background(), k.file, filepath.Join(dir(t), "target"), false, k.id)
	var refused *Refused
	if !errors.As(err, &refused) {
		if s != nil {
			s.Discard()
		}
		t.Fatalf("not refused: %v", err)
	}
}

// Naming a key means choosing between two versions, and one of them can't be seen.
func TestNamingAKeyWhenThisRootCannotBeReadIsRefused(t *testing.T) {
	k := withKey(t)
	if err := os.WriteFile(k.r.DB(), []byte(strings.Repeat("not a database ", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Stage(context.Background(), k.file, k.r.Dir, true, k.id)
	var refused *Refused
	if !errors.As(err, &refused) {
		if s != nil {
			s.Discard()
		}
		t.Fatalf("not refused: %v", err)
	}
}

// When this root can't be read, what would come back is listed as well as cautioned about.
func TestKeysAreListedWhenThisRootCannotBeRead(t *testing.T) {
	k := withKey(t)
	if err := os.WriteFile(k.r.DB(), []byte(strings.Repeat("not a database ", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := stageReport(t, k.file, k.r.Dir, true)
	oneKey(t, "active", rep.KeysActive, KeyChange{k.id, "kept", "inference:chat"})
	said := strings.Join(rep.Cautions, "\n")
	if !strings.Contains(said, "can't be read") || !strings.Contains(said, "revoked or rotated") {
		t.Errorf("cautions: %q", said)
	}
}

// A root older than key permissions can't say what its keys may do. Everything else is still
// compared, nothing fails, and the keys are listed since their permissions couldn't be compared.
func TestARootOlderThanKeyPermissionsStillCompares(t *testing.T) {
	k := withKey(t)
	current := k.rotate(t)
	k.onRoot(t, func(st *store.Store) {
		if _, err := st.DB.Exec(`ALTER TABLE api_keys DROP COLUMN scopes`); err != nil {
			t.Fatal(err)
		}
	})
	s, err := Stage(context.Background(), k.file, k.r.Dir, true)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	rep := s.Report
	if _, err := s.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	oneKey(t, "kept", rep.KeysKept, KeyChange{k.id, "kept", "rotated"})
	oneKey(t, "active", rep.KeysActive, KeyChange{k.id, "kept", "inference:chat"})
	if k.works(t, k.secret) {
		t.Error("the old secret works")
	}
	if !k.works(t, current) {
		t.Error("the current secret doesn't work")
	}
	k.onRoot(t, func(st *store.Store) {
		got, err := st.ResolveAPIKey(context.Background(), current)
		if err != nil || got == nil || strings.Join(got.Scopes, ",") != "inference:chat" {
			t.Errorf("%+v, %v", got, err)
		}
	})
}

// A key whose permissions don't read would stop every key working, so a file holding one is refused.
func TestAKeyWhosePermissionsDoNotReadIsRefused(t *testing.T) {
	k := withKey(t)
	bad := editedBackup(t, k.file, `UPDATE api_keys SET scopes = 'not a list' WHERE id = '`+k.id+`'`)
	s, err := Stage(context.Background(), bad, k.r.Dir, true)
	var refused *Refused
	if !errors.As(err, &refused) {
		if s != nil {
			s.Discard()
		}
		t.Fatalf("not refused: %v", err)
	}
	if !k.works(t, k.secret) {
		t.Error("the root changed")
	}
}

// The report can name a key and what differs, and holds nothing of the secret either way.
func TestTheKeyListsHoldNoSecret(t *testing.T) {
	k := withKey(t)
	current := k.rotate(t)
	for name, s := range map[string]func() *Report{
		"kept":         func() *Report { return stageReport(t, k.file, k.r.Dir, true) },
		"brought back": func() *Report { return stagedNaming(t, k.file, k.r.Dir, k.id) },
		"new root":     func() *Report { return stageReport(t, k.file, filepath.Join(dir(t), "target"), false) },
		"held back": func() *Report {
			return stageReport(t, addAdminKey(t, k.file, `["keys:manage"]`), filepath.Join(dir(t), "target2"), false)
		},
	} {
		b, err := json.Marshal(s())
		if err != nil {
			t.Fatal(err)
		}
		for what, secret := range map[string]string{"the old secret": k.secret, "the current secret": current,
			"the old hash": store.HashKey(k.secret), "the current hash": store.HashKey(current),
			"a held-back key's secret": adminSecret, "a held-back key's hash": store.HashKey(adminSecret)} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s: it holds %s", name, what)
			}
		}
	}
}

func stagedNaming(t *testing.T, file, target string, ids ...string) *Report {
	t.Helper()
	s, err := Stage(context.Background(), file, target, true, ids...)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	t.Cleanup(s.Discard)
	return s.Report
}

// A dry run looks. It leaves the root's keys as they are, whatever a real restore would do.
func TestStagingChangesNoKeyOnTheRoot(t *testing.T) {
	k := withKey(t)
	k.rotate(t)
	before := k.row(t, k.id)
	stageReport(t, k.file, k.r.Dir, true)
	if got := k.row(t, k.id); got != before {
		t.Errorf("the key changed:\n%s\n%s", before, got)
	}
}

// Every list comes sorted by id, so two runs read the same.
func TestKeyListsAreInOrder(t *testing.T) {
	insert := func(id string) string {
		return `INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, scopes) VALUES ('` + id + `', 'n', 'p', 'h` + id + `', 'x', 'x', '[]')`
	}
	letters := strings.Split("f,c,a,e,b,d", ",")
	k := withKey(t)
	k.onRoot(t, func(st *store.Store) {
		for _, l := range letters {
			for _, kind := range []string{"kept_", "named_"} {
				if _, err := st.DB.Exec(insert(kind + l)); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
	file := create(t, k.r, false).Output
	var edits, named []string
	for _, l := range letters {
		edits = append(edits, `UPDATE api_keys SET name = 'other' WHERE id IN ('kept_`+l+`', 'named_`+l+`')`, insert("out_"+l))
		named = append(named, "named_"+l)
	}
	bad := editedBackup(t, file, edits...)
	k.onRoot(t, func(st *store.Store) {
		for _, l := range letters {
			if _, err := st.DB.Exec(insert("lost_" + l)); err != nil {
				t.Fatal(err)
			}
		}
	})
	rep := stagedNaming(t, bad, k.r.Dir, named...)
	for prefix, list := range map[string][]KeyChange{"kept_": rep.KeysKept, "named_": rep.KeysBroughtBack, "out_": rep.KeysLeftOut, "lost_": rep.KeysLost} {
		var ids []string
		for _, l := range list {
			ids = append(ids, l.ID)
		}
		want := []string{prefix + "a", prefix + "b", prefix + "c", prefix + "d", prefix + "e", prefix + "f"}
		if strings.Join(ids, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %v", prefix, ids)
		}
	}
}

// A key that has expired is compared like any other: the root's expiry stands.
func TestAnExpiredKeyStaysExpired(t *testing.T) {
	k := withKey(t)
	past := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	k.onRoot(t, func(st *store.Store) {
		if _, err := st.DB.Exec(`UPDATE api_keys SET expires_at = ? WHERE id = ?`, past, k.id); err != nil {
			t.Fatal(err)
		}
	})
	rep, _ := restore(t, k.file, k.r.Dir, true)
	if k.works(t, k.secret) {
		t.Error("the expired key works")
	}
	oneKey(t, "kept", rep.KeysKept, KeyChange{k.id, "kept", "expiry changed"})
}
