package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// TestSessionRowsMatchTS is the shadow check's session proof (DESIGN §17),
// run only on a copy of a real root: every stored browser session gets the
// same verdict and identity from Go as from TS
// (tools/golden/session-check.ts), by its stored token hash, with no cookie.
// Only fingerprints are compared or printed.
//
//	LOCALBASE_SESSION_ROOT=<copy root> LOCALBASE_SESSION_NOW=<ms> \
//	LOCALBASE_SESSION_TS=<ts.json> go test ./internal/auth -run TestSessionRowsMatchTS
func TestSessionRowsMatchTS(t *testing.T) {
	rootDir, tsPath := os.Getenv("LOCALBASE_SESSION_ROOT"), os.Getenv("LOCALBASE_SESSION_TS")
	if rootDir == "" || tsPath == "" {
		t.Skip("set LOCALBASE_SESSION_ROOT and LOCALBASE_SESSION_TS (shadow check on a copy)")
	}
	now, err := strconv.ParseInt(os.Getenv("LOCALBASE_SESSION_NOW"), 10, 64)
	if err != nil {
		t.Fatal("LOCALBASE_SESSION_NOW must be milliseconds")
	}
	raw, err := os.ReadFile(tsPath)
	if err != nil {
		t.Fatal(err)
	}
	type verdict struct{ Row, Verdict, Owner, Issuer, Subject, Email string }
	var ts []verdict
	if err := json.Unmarshal(raw, &ts); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(rootDir + "/local-base.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	access, err := LoadAccess(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	b := NewBrowser(st, rootDir, access, slog.New(slog.NewTextHandler(io.Discard, nil)))
	issuers := b.sessionIssuers()
	fp := func(v string) string {
		sum := sha256.Sum256([]byte(v))
		return hex.EncodeToString(sum[:])[:16]
	}
	rows, err := st.DB.Query(`SELECT token_hash FROM auth_browser_sessions`)
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	for rows.Next() {
		var h string
		rows.Scan(&h)
		hashes = append(hashes, h)
	}
	rows.Close()
	if len(hashes) != len(ts) {
		t.Fatalf("Go sees %d sessions, TS %d", len(hashes), len(ts))
	}
	byRow := map[string]verdict{}
	for _, v := range ts {
		byRow[v.Row] = v
	}
	for _, h := range hashes {
		s, got, err := checkSession(context.Background(), st, h, issuers, now)
		if err != nil {
			t.Fatal(err)
		}
		g := verdict{Row: fp(h), Verdict: got, Owner: fp(""), Issuer: fp(""), Subject: fp(""), Email: fp("")}
		if s != nil {
			g.Owner, g.Issuer, g.Subject, g.Email = fp(s.OwnerID), fp(s.Identity.Issuer), fp(s.Identity.Subject), fp(s.Identity.VerifiedEmail)
		}
		want, ok := byRow[g.Row]
		switch {
		case !ok:
			t.Errorf("session %s: TS has no verdict", g.Row)
		case got != want.Verdict && !(got == "issuer" && want.Verdict == "issuer"):
			t.Errorf("session %s: Go %s, TS %s", g.Row, got, want.Verdict)
		case s != nil && (g.Owner != want.Owner || g.Issuer != want.Issuer || g.Subject != want.Subject || g.Email != want.Email):
			t.Errorf("session %s: identity fingerprints differ", g.Row)
		default:
			t.Logf("session %s: %s in both (identity fingerprints equal)", g.Row, got)
		}
	}
}
