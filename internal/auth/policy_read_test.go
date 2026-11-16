package auth

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// holdWriteLock takes the database's write lock on a connection of its own, as any writer does.
func holdWriteLock(t *testing.T, db *sql.DB) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	return func() {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		_ = conn.Close()
	}
}

// Looking up what a signed-in person may do doesn't wait for whoever is writing.
func TestPolicyLookupDoesNotTakeTheWriteLock(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const issuer = "https://idp.example"
	if err := ApplyPolicy(ctx, st, StarterPolicy("person@example.com", issuer)); err != nil {
		t.Fatal(err)
	}
	release := holdWriteLock(t, st.DB)
	defer release()
	started := time.Now()
	d, err := ResolvePolicy(ctx, st, Identity{Issuer: issuer, Subject: "s", VerifiedEmail: "person@example.com"}, true)
	if err != nil {
		t.Fatalf("with a writer holding the lock: %v", err)
	}
	if len(d.Permissions) == 0 {
		t.Errorf("%+v", d)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("waited %v for a lock it doesn't need", took)
	}
}

// An invitation is still claimed, though the first look can't do it. A person with nothing to claim
// writes nothing.
func TestAClaimGoesRoundAgainWithTheLock(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const issuer = "https://idp.example"
	if err := ApplyPolicy(ctx, st, StarterPolicy("owner@example.com", issuer)); err != nil {
		t.Fatal(err)
	}
	const email = "invited@example.com"
	user, err := InviteUser(ctx, st, email, []string{"admin"}, issuer)
	if err != nil {
		t.Fatal(err)
	}
	if user.Status != "pending" {
		t.Fatalf("%+v", user)
	}
	id := Identity{Issuer: issuer, Subject: "invited-subject", VerifiedEmail: email}

	// Without claiming, the record stays pending.
	if _, err := ResolvePolicy(ctx, st, id, false); err != nil {
		t.Fatal(err)
	}
	if got, err := userByEmail(ctx, st.DB, email); err != nil || got.Status != "pending" {
		t.Fatalf("after a look: %+v %v", got, err)
	}
	d, err := ResolvePolicy(ctx, st, id, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Permissions) == 0 {
		t.Errorf("after claiming: %+v", d)
	}
	if got, err := userByEmail(ctx, st.DB, email); err != nil || got.Status != "active" {
		t.Errorf("after claiming: %+v %v", got, err)
	}
}

// The read-only look stops at anything it would have to write, and has written nothing: a write in a
// deferred transaction is what gets SQLITE_BUSY with no wait at all.
func TestTheReadOnlyLookStopsAtAClaim(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const issuer = "https://idp.example"
	if err := ApplyPolicy(ctx, st, StarterPolicy("owner@example.com", issuer)); err != nil {
		t.Fatal(err)
	}
	const email = "invited@example.com"
	if _, err := InviteUser(ctx, st, email, []string{"admin"}, issuer); err != nil {
		t.Fatal(err)
	}
	id := Identity{Issuer: issuer, Subject: "invited-subject", VerifiedEmail: email}
	readOnly := &sql.TxOptions{ReadOnly: true}
	if _, err := evaluatePolicy(ctx, st, id, true, readOnly); !errors.Is(err, errPolicyWrites) {
		t.Errorf("an invitation to claim: %v", err)
	}
	if got, err := userByEmail(ctx, st.DB, email); err != nil || got.Status != "pending" {
		t.Errorf("the look changed something: %+v %v", got, err)
	}
	// Claimed, then pending again: the second place it would write.
	if _, err := ResolvePolicy(ctx, st, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE auth_users SET status = 'pending' WHERE id = (SELECT user_id FROM auth_user_emails WHERE email = ?)`, email); err != nil {
		t.Fatal(err)
	}
	if _, err := evaluatePolicy(ctx, st, id, true, readOnly); !errors.Is(err, errPolicyWrites) {
		t.Errorf("a pending record with its identity: %v", err)
	}
	if got, err := userByEmail(ctx, st.DB, email); err != nil || got.Status != "pending" {
		t.Errorf("the look changed something: %+v %v", got, err)
	}
	d, err := ResolvePolicy(ctx, st, id, true)
	if err != nil || len(d.Permissions) == 0 {
		t.Errorf("then the claim: %+v %v", d, err)
	}
}
