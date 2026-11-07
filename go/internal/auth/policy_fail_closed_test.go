package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// policyStore is a store holding a policy with two roles, chat and video.
func policyStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	policy := &Policy{Roles: []Role{
		{Name: "chat", Description: "Chat", Permissions: []string{"inference:chat", "access:manage"}},
		{Name: "video", Description: "Video", Permissions: []string{"inference:video"}},
	}, Bindings: []Binding{{Kind: "subject", Role: "chat", Issuer: "https://id.example.com", Subject: "owner"}}}
	if err := ApplyPolicy(context.Background(), st, policy); err != nil {
		t.Fatal(err)
	}
	return st
}

// unreadable makes a table one that every read of fails, as any error from the database would.
func unreadable(t *testing.T, st *store.Store, table string) {
	t.Helper()
	if _, err := st.DB.Exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_kept`); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, st *store.Store, query string) int {
	t.Helper()
	var n int
	if err := st.DB.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Applying a policy that drops a role asks whether a managed user holds it. When it can't tell, it stops:
// read as "nobody", the role would go, and whoever holds it with it.
func TestApplyPolicyStopsWhenItCannotTellWhoHoldsARole(t *testing.T) {
	st := policyStore(t)
	unreadable(t, st, "auth_user_roles")
	chatOnly := &Policy{Roles: []Role{{Name: "chat", Description: "Chat", Permissions: []string{"inference:chat", "access:manage"}}},
		Bindings: []Binding{{Kind: "subject", Role: "chat", Issuer: "https://id.example.com", Subject: "owner"}}}
	if err := ApplyPolicy(context.Background(), st, chatOnly); err == nil {
		t.Error("the policy was applied")
	}
	if n := count(t, st, `SELECT COUNT(*) FROM auth_roles WHERE name = 'video'`); n != 1 {
		t.Errorf("the role is gone (%d)", n)
	}
}

// Clearing the policy asks whether managed users exist. When it can't tell, it stops: read as "none", it
// would remove every role and binding.
func TestClearPolicyStopsWhenItCannotTellWhetherUsersExist(t *testing.T) {
	st := policyStore(t)
	unreadable(t, st, "auth_users")
	cleared, err := ClearPolicy(context.Background(), st)
	if err == nil || cleared {
		t.Errorf("cleared %v, %v", cleared, err)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM auth_roles`); n != 2 {
		t.Errorf("roles left: %d", n)
	}
	if n := count(t, st, `SELECT COUNT(*) FROM auth_settings`); n != 1 {
		t.Errorf("settings left: %d", n)
	}
}
