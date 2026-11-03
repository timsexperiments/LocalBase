package auth

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// failTableWrites makes every write of a kind to a table fail, as a database that stays busy would, while
// reads go on working. The result is only known from the log.
func failTableWrites(t *testing.T, st *store.Store, kind, table string) *strings.Builder {
	t.Helper()
	var logged strings.Builder
	st.Log = slog.New(slog.NewJSONHandler(&logged, nil))
	if _, err := st.DB.Exec(`CREATE TRIGGER planted BEFORE ` + kind + ` ON ` + table + ` BEGIN SELECT RAISE(ABORT, 'planted'); END`); err != nil {
		t.Fatal(err)
	}
	return &logged
}

// Writes that nobody is waiting on still say when they fail, and the answer to the person stands.
func TestSessionWritesThatFailAreNotSilent(t *testing.T) {
	ctx := context.Background()
	const issuer = "https://idp.example"
	issuers := map[string]bool{issuer: true}
	id := Identity{Issuer: issuer, Subject: "s"}
	old := time.Now().Add(-2 * sessionTouchEvery).UnixMilli()

	t.Run("browser touch", func(t *testing.T) {
		st := testStore(t)
		token, err := CreateSession(ctx, st, "owner", id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`UPDATE auth_browser_sessions SET last_seen_at = ?`, old); err != nil {
			t.Fatal(err)
		}
		logged := failTableWrites(t, st, "UPDATE", "auth_browser_sessions")
		if s, err := FindSession(ctx, st, token, issuers); err != nil || s == nil {
			t.Fatalf("the session should stand: %v %v", s, err)
		}
		if !strings.Contains(logged.String(), "store.session-touch-write-failed") {
			t.Errorf("%q", logged.String())
		}
	})
	t.Run("browser tidy of an expired session", func(t *testing.T) {
		st := testStore(t)
		token, err := CreateSession(ctx, st, "owner", id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`UPDATE auth_browser_sessions SET idle_expires_at = 1`); err != nil {
			t.Fatal(err)
		}
		logged := failTableWrites(t, st, "DELETE", "auth_browser_sessions")
		if s, err := FindSession(ctx, st, token, issuers); err != nil || s != nil {
			t.Fatalf("an expired session is refused: %v %v", s, err)
		}
		if !strings.Contains(logged.String(), "store.session-tidy-write-failed") {
			t.Errorf("%q", logged.String())
		}
		logged.Reset()
		if _, err := CreateSession(ctx, st, "owner", id); err != nil {
			t.Fatalf("a new session isn't held up by tidying: %v", err)
		}
		if !strings.Contains(logged.String(), "store.session-tidy-write-failed") {
			t.Errorf("at create: %q", logged.String())
		}
	})
	t.Run("local touch", func(t *testing.T) {
		st := testStore(t)
		token, _, err := CreateLocalSession(ctx, st)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`UPDATE local_sessions SET last_seen_at = ?`, old); err != nil {
			t.Fatal(err)
		}
		logged := failTableWrites(t, st, "UPDATE", "local_sessions")
		if s, err := FindLocalSession(ctx, st, token); err != nil || s == nil {
			t.Fatalf("the session should stand: %v %v", s, err)
		}
		if !strings.Contains(logged.String(), "store.session-touch-write-failed") {
			t.Errorf("%q", logged.String())
		}
	})
	t.Run("local tidy", func(t *testing.T) {
		st := testStore(t)
		token, _, err := CreateLocalSession(ctx, st)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.Exec(`UPDATE local_sessions SET idle_expires_at = 1`); err != nil {
			t.Fatal(err)
		}
		logged := failTableWrites(t, st, "DELETE", "local_sessions")
		if s, err := FindLocalSession(ctx, st, token); err != nil || s != nil {
			t.Fatalf("an expired session is refused: %v %v", s, err)
		}
		if !strings.Contains(logged.String(), "store.session-tidy-write-failed") {
			t.Errorf("%q", logged.String())
		}
		logged.Reset()
		if _, _, err := CreateLocalSession(ctx, st); err != nil {
			t.Fatalf("a new session isn't held up by tidying: %v", err)
		}
		if !strings.Contains(logged.String(), "store.session-tidy-write-failed") {
			t.Errorf("at create: %q", logged.String())
		}
	})
}
