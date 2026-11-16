package auth

import (
	"context"
	"testing"
	"time"
)

// Changing nothing writes nothing: a person's roles set to the same set (in any order), or their
// status to what it is, leaves their record as it was, updatedAt included, and touches no row.
// (Triggers on the test store record any write to the user's row or their roles.)
func TestChangingNothingWritesNothing(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	const issuer = "https://a.example.com"
	if err := ApplyPolicy(ctx, st, StarterPolicy("owner@example.com", issuer)); err != nil {
		t.Fatal(err)
	}
	if _, err := InviteUser(ctx, st, "friend@example.com", []string{"member", "admin"}, issuer); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := SetUserStatus(ctx, st, "friend@example.com", "disabled"); err != nil || !changed {
		t.Fatalf("a real change: %v, %v", changed, err)
	}
	for _, q := range []string{
		`CREATE TABLE probe_writes (what TEXT)`,
		`CREATE TRIGGER probe_user AFTER UPDATE ON auth_users BEGIN INSERT INTO probe_writes VALUES ('user'); END`,
		`CREATE TRIGGER probe_roles AFTER DELETE ON auth_user_roles BEGIN INSERT INTO probe_writes VALUES ('roles'); END`,
	} {
		if _, err := st.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	writes := func() int {
		t.Helper()
		var n int
		if err := st.DB.QueryRowContext(ctx, `SELECT count(*) FROM probe_writes`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before, err := userByEmail(ctx, st.DB, "friend@example.com")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond) // so a write would move updatedAt, at its millisecond resolution

	if u, changed, err := ReplaceUserRoles(ctx, st, "friend@example.com", []string{"admin", "member"}); err != nil || changed || u.UpdatedAt != before.UpdatedAt {
		t.Errorf("the same roles in another order: changed %v, err %v, updatedAt %s (was %s)", changed, err, u.UpdatedAt, before.UpdatedAt)
	}
	if u, changed, err := SetUserStatus(ctx, st, "friend@example.com", "disabled"); err != nil || changed || u.UpdatedAt != before.UpdatedAt {
		t.Errorf("the same status: changed %v, err %v, updatedAt %s (was %s)", changed, err, u.UpdatedAt, before.UpdatedAt)
	}
	after, err := userByEmail(ctx, st.DB, "friend@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("updatedAt moved: %s, was %s", after.UpdatedAt, before.UpdatedAt)
	}
	if n := writes(); n != 0 {
		t.Errorf("changing nothing wrote %d times", n)
	}
	// A real change still writes.
	if _, changed, err := SetUserStatus(ctx, st, "friend@example.com", "active"); err != nil || !changed {
		t.Errorf("a real change: %v, %v", changed, err)
	}
	if n := writes(); n == 0 {
		t.Error("a real change wrote nothing, so the probe proves nothing")
	}
}
