package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// invitedByMagicLink is a store with one address invited to sign in by magic link.
func invitedByMagicLink(t *testing.T) (*Browser, string) {
	t.Helper()
	b := magicBrowser(t)
	if _, err := InviteUser(context.Background(), b.Store, "friend@example.com", []string{"chat"}, MagicIssuer(testOrigin)); err != nil {
		t.Fatal(err)
	}
	return b, "friend@example.com"
}

// ageLinks makes every pending link as old as by.
func ageLinks(t *testing.T, st *store.Store, by time.Duration) {
	t.Helper()
	format := "2006-01-02T15:04:05.000Z"
	var created, expires string
	if err := st.DB.QueryRow(`SELECT created_at, expires_at FROM auth_magic_link_tokens`).Scan(&created, &expires); err != nil {
		t.Fatal(err)
	}
	c, _ := time.Parse(time.RFC3339Nano, created)
	e, _ := time.Parse(time.RFC3339Nano, expires)
	if _, err := st.DB.Exec(`UPDATE auth_magic_link_tokens SET created_at = ?, expires_at = ?`, c.Add(-by).UTC().Format(format), e.Add(-by).UTC().Format(format)); err != nil {
		t.Fatal(err)
	}
}

// A link that is still unused and unexpired stays the one that works: asking again sends nothing, so
// whoever asks can't replace the link the person was sent, or send them another every minute.
func TestAnUnusedLinkIsNotReplacedBeforeItExpires(t *testing.T) {
	b, email := invitedByMagicLink(t)
	ctx := context.Background()
	first, _, err := IssueMagicLink(ctx, b.Store, email, MagicIssuer(testOrigin))
	if err != nil || first == "" {
		t.Fatalf("first link: %q, %v", first, err)
	}
	ageLinks(t, b.Store, 14*time.Minute)
	again, _, err := IssueMagicLink(ctx, b.Store, email, MagicIssuer(testOrigin))
	if err != nil || again != "" {
		t.Fatalf("a second link while the first works: %q, %v", again, err)
	}
	if id, err := ConsumeMagicLink(ctx, b.Store, first, MagicIssuer(testOrigin)); err != nil || id == nil {
		t.Errorf("the first link stopped working: %+v, %v", id, err)
	}
}

// Once the link has expired, or has been used, asking again sends a new one.
func TestAnExpiredOrUsedLinkIsReplaced(t *testing.T) {
	b, email := invitedByMagicLink(t)
	ctx := context.Background()
	if tok, _, err := IssueMagicLink(ctx, b.Store, email, MagicIssuer(testOrigin)); err != nil || tok == "" {
		t.Fatalf("first link: %q, %v", tok, err)
	}
	ageLinks(t, b.Store, 16*time.Minute)
	used, _, err := IssueMagicLink(ctx, b.Store, email, MagicIssuer(testOrigin))
	if err != nil || used == "" {
		t.Fatalf("after the first expired: %q, %v", used, err)
	}
	if id, err := ConsumeMagicLink(ctx, b.Store, used, MagicIssuer(testOrigin)); err != nil || id == nil {
		t.Fatalf("using it: %+v, %v", id, err)
	}
	if tok, _, err := IssueMagicLink(ctx, b.Store, email, MagicIssuer(testOrigin)); err != nil || tok == "" {
		t.Errorf("after the last was used: %q, %v", tok, err)
	}
}

// A link that couldn't be delivered is dropped, so asking again at once sends a new one.
func TestAFailedDeliveryLetsAnImmediateRetrySendOne(t *testing.T) {
	b, email := invitedByMagicLink(t)
	var sent []string
	askForLink(b, email, func(_, link string) error { sent = append(sent, link); return errors.New("the mail server refused it") })
	waitForMagicWork(t, b)
	askForLink(b, email, func(_, link string) error { sent = append(sent, link); return nil })
	waitForMagicWork(t, b)
	if len(sent) != 2 || sent[0] == sent[1] {
		t.Fatalf("links tried: %d", len(sent))
	}
	token := sent[1][strings.Index(sent[1], "#")+1:]
	if id, err := ConsumeMagicLink(context.Background(), b.Store, token, MagicIssuer(testOrigin)); err != nil || id == nil {
		t.Errorf("the retry's link: %+v, %v", id, err)
	}
}

// The reply is the same for every address, whether or not it can sign in, and whether or not a link is
// sent. It comes before the address is even looked up, so how long it takes can't tell either.
func TestTheReplyIsTheSameForEveryAddress(t *testing.T) {
	b, email := invitedByMagicLink(t)
	release := make(chan struct{})
	deliver := func(_, _ string) error { <-release; return nil }
	replies := map[string]string{}
	for name, addr := range map[string]string{"invited": email, "unknown": "nobody@example.com", "not an address": "nobody"} {
		replies[name] = askForLink(b, addr, deliver)
	}
	replies["invited, again"] = askForLink(b, email, deliver)
	close(release)
	waitForMagicWork(t, b)
	want := replies["invited"]
	if !strings.HasPrefix(want, "200 ") || !strings.Contains(want, "If that address can sign in, a link is on its way. It works once, for 15 minutes, and asking again before then doesn’t send another.") {
		t.Fatalf("reply: %s", want)
	}
	for name, got := range replies {
		if got != want {
			t.Errorf("%s: %s", name, got)
		}
	}
}
