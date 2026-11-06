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
	"sync"
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
	return askForLinkFrom(b, email, testOrigin, deliver)
}

func askForLinkFrom(b *Browser, email, origin string, deliver func(to, link string) error) string {
	r := httptest.NewRequest("POST", MagicRequestPath+"?provider=email", strings.NewReader(url.Values{"email": {email}}.Encode()))
	r.Host = "lb.example.com"
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	b.RequestMagicLink(w, r, deliver)
	return fmt.Sprint(w.Code, " ", w.Body.String())
}

// A loaded machine has taken over 4 s to finish one of these. The deadline is only for a test
// that has gone wrong.
const magicWorkDeadline = 30 * time.Second

// waitForMagicWork waits until nothing a request started is still running, by taking every slot.
// A request takes its slot before it returns and gives it back after delivering, so after this
// "no link" means none is coming, at any speed. Taking the slots, not reading how many are taken,
// is also what orders the test after the work.
func waitForMagicWork(t *testing.T, b *Browser) {
	t.Helper()
	deadline := time.After(magicWorkDeadline)
	for taken := range cap(b.magicWork) {
		select {
		case b.magicWork <- struct{}{}:
		case <-deadline:
			t.Fatalf("%d workers still busy", cap(b.magicWork)-taken)
		}
	}
	for range cap(b.magicWork) {
		<-b.magicWork
	}
}

// What a refusal means is checked once the work has finished. A fixed wait can pass without
// having checked anything, if the worker takes longer than the wait.
func TestMagicLinkRefusalsAreExact(t *testing.T) {
	b := magicBrowser(t)
	if _, err := InviteUser(context.Background(), b.Store, "friend@example.com", []string{"chat"}, MagicIssuer(testOrigin)); err != nil {
		t.Fatal(err)
	}
	ask := func(email, origin string) []string {
		t.Helper()
		links := make(chan string, magicLinkWorkers)
		askForLinkFrom(b, email, origin, func(_, link string) error { links <- link; return nil })
		waitForMagicWork(t, b)
		close(links)
		var got []string
		for link := range links {
			got = append(got, link)
		}
		return got
	}
	if got := ask("stranger@example.com", testOrigin); len(got) != 0 {
		t.Errorf("a link for an address that can't sign in: %q", got)
	}
	if got := ask("friend@example.com", "https://evil.example.com"); len(got) != 0 {
		t.Errorf("a link for a form from another origin: %q", got)
	}
	if got := ask("FRIEND@example.com", testOrigin); len(got) != 1 {
		t.Fatalf("%d links for someone invited", len(got))
	}
	if got := ask("friend@example.com", testOrigin); len(got) != 0 {
		t.Errorf("a second link inside the cooldown: %q", got)
	}
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
	stop := sync.OnceFunc(func() { close(release) })
	defer stop()
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
	waitForMagicWork(t, b) // that address can't sign in, so it has nothing to deliver
	person := func(i int) string { return fmt.Sprintf("person%d@example.com", i) }
	// One at a time until every worker is busy, at any speed.
	for i := range magicLinkWorkers {
		if got := askForLink(b, person(i), slow); got != want {
			t.Errorf("a different page: %.200s", got)
		}
		deadline := time.Now().Add(magicWorkDeadline)
		for running.Load() != int64(i+1) {
			if time.Now().After(deadline) {
				t.Fatalf("%d running, want %d", running.Load(), i+1)
			}
			time.Sleep(time.Millisecond)
		}
	}
	// The rest meet a full house.
	for i := magicLinkWorkers; i < 20; i++ {
		if got := askForLink(b, person(i), slow); got != want {
			t.Errorf("a different page: %.200s", got)
		}
	}
	// Anything let in by mistake gets to delivery soon. A slow machine can only hide that failure
	// here, and can't cause one.
	time.Sleep(200 * time.Millisecond)
	if n := running.Load(); n != magicLinkWorkers {
		t.Errorf("%d running, want %d", n, magicLinkWorkers)
	}
	stop()
	waitForMagicWork(t, b)
	if m := most.Load(); m > magicLinkWorkers {
		t.Errorf("%d ran at once", m)
	}
	if d := delivered.Load(); d != magicLinkWorkers {
		t.Errorf("%d delivered, want %d: the rest were dropped", d, magicLinkWorkers)
	}
	// All clear: the next person gets a link.
	links := make(chan string, 1)
	askForLink(b, "after@example.com", func(_, link string) error { links <- link; return nil })
	waitForMagicWork(t, b)
	select {
	case link := <-links:
		if !strings.Contains(link, MagicCallbackPath) {
			t.Errorf("link %q", link)
		}
	default:
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
	waitForMagicWork(t, b)
	// The database driver's helpers come and go at the machine's pace, and a rise of 34 has been
	// seen. Without the bound it's hundreds on one P and thousands otherwise. How many workers
	// run is pinned exactly by TestMagicLinkRequestsAreBounded.
	if p := peak.Load(); p > int64(before+200) {
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
	for i := range 200 {
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
