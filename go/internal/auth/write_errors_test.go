package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// failWrites makes SQLite refuse an operation on a table, so a write fails on demand.
func failWrites(t *testing.T, st *store.Store, table, op string) {
	t.Helper()
	q := `CREATE TRIGGER fail_` + table + ` BEFORE ` + op + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`
	if _, err := st.DB.Exec(q); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.DB.Exec(`DROP TRIGGER IF EXISTS fail_` + table) })
}

func invited(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st := testStore(t)
	p := &Policy{Roles: []Role{{Name: "chat", Description: "Chat", Permissions: []string{"inference:chat", "access:manage"}},
		{Name: "view", Description: "View", Permissions: []string{"models:read"}}},
		Bindings: []Binding{{Kind: "subject", Role: "chat", Issuer: "https://id.example.com", Subject: "owner"}}}
	if err := ApplyPolicy(ctx, st, p); err != nil {
		t.Fatal(err)
	}
	if _, err := InviteUser(ctx, st, "friend@example.com", []string{"chat"}); err != nil {
		t.Fatal(err)
	}
	return st
}

// A link that could not be used up must not sign anyone in: it would work again.
func TestMagicLinkNotConsumedDoesNotSignIn(t *testing.T) {
	ctx := context.Background()
	st := invited(t)
	token, _, err := IssueMagicLink(ctx, st, "friend@example.com")
	if err != nil || token == "" {
		t.Fatalf("issue: %q %v", token, err)
	}
	failWrites(t, st, "auth_magic_link_tokens", "DELETE")
	id, err := ConsumeMagicLink(ctx, st, token, "https://i")
	if id != nil {
		t.Errorf("signed in with a link that stayed usable: %+v", id)
	}
	if err == nil {
		t.Error("the failure was not reported")
	}
}

// New roles must not land on top of old ones: a demotion would leave the old role.
func TestReplaceUserRolesReportsAFailedDelete(t *testing.T) {
	ctx := context.Background()
	st := invited(t)
	failWrites(t, st, "auth_user_roles", "DELETE")
	u, err := ReplaceUserRoles(ctx, st, "friend@example.com", []string{"view"})
	if err == nil {
		t.Fatalf("no error: %+v", u)
	}
	list, lerr := ListUsers(ctx, st)
	if lerr != nil || len(list) != 1 || strings.Join(list[0].Roles, ",") != "chat" {
		t.Errorf("roles changed despite the failure: %+v %v", list, lerr)
	}
}

// A failed sign-out leaves the session valid, so whoever signs out must hear about it.
func TestDeleteSessionReportsFailure(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	token, err := CreateSession(ctx, st, "owner", Identity{Issuer: "https://i", Subject: "s"})
	if err != nil {
		t.Fatal(err)
	}
	failWrites(t, st, "auth_browser_sessions", "DELETE")
	if err := DeleteSession(ctx, st, token); err == nil {
		t.Error("a failed delete was not reported")
	}
}

func TestRevokeAndClearMagicLinksReportFailure(t *testing.T) {
	ctx := context.Background()
	st := invited(t)
	token, _, err := IssueMagicLink(ctx, st, "friend@example.com")
	if err != nil {
		t.Fatal(err)
	}
	failWrites(t, st, "auth_magic_link_tokens", "DELETE")
	if err := RevokeMagicLink(ctx, st, token); err == nil {
		t.Error("revoke: not reported")
	}
	if err := ClearMagicLinks(ctx, st); err == nil {
		t.Error("clear: not reported")
	}
}
