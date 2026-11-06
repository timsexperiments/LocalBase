package auth

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Applying a policy reads the roles there are, to keep their ids. One it can't read must stop it: it
// would look new, get a new id, and leave whoever holds it behind.
func TestApplyPolicyStopsOnARoleItCannotRead(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	policy := &Policy{Roles: []Role{{Name: "chat", Description: "Chat", Permissions: []string{"inference:chat", "access:manage"}}},
		Bindings: []Binding{{Kind: "subject", Role: "chat", Issuer: "https://id.example.com", Subject: "owner"}}}
	if err := ApplyPolicy(ctx, st, policy); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`PRAGMA foreign_keys = OFF`,
		`ALTER TABLE auth_roles RENAME TO auth_roles_kept`,
		`CREATE TABLE auth_roles (id text PRIMARY KEY, name text, description text, created_at text, updated_at text)`,
		`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('broken', NULL, '', '', '')`,
	} {
		if _, err := st.DB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	err = ApplyPolicy(ctx, st, policy)
	if err == nil || !strings.Contains(err.Error(), "Scan") {
		t.Errorf("a role that couldn't be read: %v", err)
	}
}
