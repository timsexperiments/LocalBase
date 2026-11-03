package auth

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func magicBrowser(t *testing.T) *Browser {
	t.Helper()
	st := testStore(t)
	b := browserFor(t, st, Registration{Kind: "magic-link", ID: "email", Name: "Email"})
	admin := Role{Name: "admin", Description: "Admin", Permissions: []string{"access:manage"}}
	owner := Binding{Kind: "subject", Role: "admin", Issuer: "https://id.example.com", Subject: "owner"}
	if err := ApplyPolicy(context.Background(), st, &Policy{Roles: []Role{{Name: "chat", Description: "Chat", Permissions: []string{"inference:chat"}}, admin},
		Bindings: []Binding{owner}}); err != nil {
		t.Fatal(err)
	}
	return b
}

func askForLink(b *Browser, email string, deliver func(to, link string) error) string {
	r := httptest.NewRequest("POST", MagicRequestPath+"?provider=email", strings.NewReader(url.Values{"email": {email}}.Encode()))
	r.Host = "lb.example.com"
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	b.RequestMagicLink(w, r, deliver)
	return fmt.Sprint(w.Code, " ", w.Body.String())
}

// Anyone can post here, so what a post starts is bounded. Whoever asks gets the same page,
// and a person asking after the flood gets their link.
func TestMagicLinkRequestsAreBounded(t *testing.T) {
	b := magicBrowser(t)
	ctx := context.Background()
	magic := MagicIssuer(testOrigin)
	for i := range 20 {
		if _, err := InviteUser(ctx, b.Store, fmt.Sprintf("person%d@example.com", i), []string{"chat"}, magic); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := InviteUser(ctx, b.Store, "after@example.com", []string{"chat"}, magic); err != nil {
		t.Fatal(err)
	}
	var running, most, delivered atomic.Int64
	release := make(chan struct{})
	slow := func(string, string) error {
		n := running.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		delivered.Add(1)
		return nil
	}
	want := askForLink(b, "nobody-at-all@example.com", slow)
	// Spaced out until every worker is busy, then the rest meet a full house.
	for i := range 20 {
		if got := askForLink(b, fmt.Sprintf("person%d@example.com", i), slow); got != want {
			t.Errorf("a different page: %.200s", got)
		}
		if running.Load() < magicLinkWorkers {
			time.Sleep(50 * time.Millisecond)
		}
	}
	time.Sleep(200 * time.Millisecond) // anything more that was started has started by now
	if n := running.Load(); n != magicLinkWorkers {
		t.Errorf("%d running, want %d", n, magicLinkWorkers)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for running.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m := most.Load(); m > magicLinkWorkers {
		t.Errorf("%d ran at once", m)
	}
	// All clear: the next person gets a link.
	links := make(chan string, 1)
	deadline = time.Now().Add(5 * time.Second)
	for len(b.magicWork) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	askForLink(b, "after@example.com", func(_, link string) error { links <- link; return nil })
	select {
	case link := <-links:
		if !strings.Contains(link, MagicCallbackPath) {
			t.Errorf("link %q", link)
		}
	case <-time.After(5 * time.Second):
		t.Error("no link came after the flood")
	}
}

// A flood of addresses, real-looking or not, starts almost nothing.
func TestAFloodOfMagicLinkRequestsStartsLittle(t *testing.T) {
	b := magicBrowser(t)
	before := runtime.NumGoroutine()
	var peak atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if n := int64(runtime.NumGoroutine()); n > peak.Load() {
				peak.Store(n)
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	for i := range 5000 {
		askForLink(b, fmt.Sprintf("a%d@example.com", i), func(string, string) error { return nil })
		if i%3 == 0 {
			askForLink(b, "not an address", func(string, string) error { return nil })
		}
	}
	close(stop)
	<-done
	if p := peak.Load(); p > int64(before+magicLinkWorkers+8) {
		t.Errorf("peak %d goroutines from %d", p, before)
	}
}

// Dropping is on record, without letting the flood write to the log.
func TestDroppedMagicLinkRequestsAreLoggedSparingly(t *testing.T) {
	b := magicBrowser(t)
	var logs bytes.Buffer
	b.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	for range magicLinkWorkers {
		b.magicWork <- struct{}{}
	}
	for i := range 1000 {
		askForLink(b, fmt.Sprintf("a%d@example.com", i), func(string, string) error { return nil })
	}
	if n := strings.Count(logs.String(), `"event":"auth.magic-link-dropped"`); n != 1 {
		t.Errorf("%d log lines: %.300s", n, logs.String())
	}
	if strings.Contains(logs.String(), "@example.com") {
		t.Error("an address reached the log")
	}
}

// Something that can't be an address costs nothing, so it isn't dropped either.
func TestAMalformedMagicLinkRequestTakesNoSlot(t *testing.T) {
	b := magicBrowser(t)
	var logs bytes.Buffer
	b.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	for range magicLinkWorkers {
		b.magicWork <- struct{}{}
	}
	askForLink(b, "not an address", func(string, string) error { return nil })
	if strings.Contains(logs.String(), "auth.magic-link-dropped") {
		t.Errorf("logged as dropped: %s", logs.String())
	}
}
