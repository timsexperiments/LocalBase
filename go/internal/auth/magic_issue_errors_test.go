package auth

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
)

// breakMagicLinks takes away the table IssueMagicLink reads first, so the database fails partway
// through. It's put back when the test ends, for the store's own cleanup.
func breakMagicLinks(t *testing.T, b *Browser) {
	t.Helper()
	if _, err := b.Store.DB.Exec(`ALTER TABLE auth_user_emails RENAME TO auth_user_emails_gone`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Store.DB.Exec(`ALTER TABLE auth_user_emails_gone RENAME TO auth_user_emails`) })
}

func TestAnAddressNobodyHasIsNotAnError(t *testing.T) {
	b := magicBrowser(t)
	var logs safeBuffer
	b.Log = slog.New(slog.NewTextHandler(&logs, nil))
	token, to, err := IssueMagicLink(context.Background(), b.Store, "nobody@example.com", MagicIssuer(testOrigin))
	if token != "" || to != "" || err != nil {
		t.Errorf("%q %q %v", token, to, err)
	}
	var delivered atomic.Int64
	askForLink(b, "nobody@example.com", func(string, string) error { delivered.Add(1); return nil })
	waitForMagicWork(t, b)
	if n := delivered.Load(); n != 0 {
		t.Errorf("%d emails went out", n)
	}
	if strings.Contains(logs.String(), "auth.magic-link-issue-failed") {
		t.Errorf("it was logged as a failure: %s", logs.String())
	}
}

func TestADatabaseFailureIsNotTakenForAnAddressNobodyHas(t *testing.T) {
	b := magicBrowser(t)
	breakMagicLinks(t, b)
	token, _, err := IssueMagicLink(context.Background(), b.Store, "nobody@example.com", MagicIssuer(testOrigin))
	if err == nil {
		t.Error("no error from a failing database")
	}
	if token != "" {
		t.Error("a link was issued all the same")
	}
}

// A person's link not coming shows up in the log, once however many ask, without the address. The
// page doesn't change, so it says nothing about who has an account.
func TestAFailedLinkIsLoggedSparingly(t *testing.T) {
	b := magicBrowser(t)
	var logs safeBuffer
	b.Log = slog.New(slog.NewTextHandler(&logs, nil))
	breakMagicLinks(t, b)
	const address = "someone-private@example.com"
	var delivered atomic.Int64
	deliver := func(string, string) error { delivered.Add(1); return nil }
	for range 3 {
		if page := askForLink(b, address, deliver); !strings.Contains(page, "Check your email") {
			t.Errorf("the page changed: %.200s", page)
		}
		waitForMagicWork(t, b)
	}
	got := logs.String()
	if n := strings.Count(got, "auth.magic-link-issue-failed"); n != 1 {
		t.Errorf("%d lines for 3 failures: %s", n, got)
	}
	if strings.Contains(got, address) {
		t.Error("the log has the address")
	}
	if n := delivered.Load(); n != 0 {
		t.Errorf("%d emails went out", n)
	}
}

// Someone an admin has disabled gets no link, and that isn't a failure. Someone else invited the
// same way does get one, so it's the status that stopped it.
func TestADisabledPersonGetsNoLink(t *testing.T) {
	b, ctx := magicFixture(t)
	issuer := MagicIssuer(testOrigin)
	if _, err := InviteUser(ctx, b.Store, "pal@example.com", []string{"chat"}, issuer); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SetUserStatus(ctx, b.Store, "friend@example.com", "disabled"); err != nil {
		t.Fatal(err)
	}
	if token, _, err := IssueMagicLink(ctx, b.Store, "friend@example.com", issuer); token != "" || err != nil {
		t.Errorf("disabled: %q %v", token, err)
	}
	if token, _, err := IssueMagicLink(ctx, b.Store, "pal@example.com", issuer); token == "" || err != nil {
		t.Errorf("invited: %q %v", token, err)
	}
}
