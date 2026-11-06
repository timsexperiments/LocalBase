package auth

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func oidcBrowser(t *testing.T) (*fakeIdP, *Browser) {
	t.Helper()
	f := newFakeIdP(t)
	reg := Registration{Kind: "oidc", ID: "idp", Name: "IdP", Issuer: f.srv.URL, ClientID: "cid",
		ClientAuthentication: &ClientAuth{Kind: "client-secret-basic", ClientSecret: "shh"}}
	b := browserFor(t, testStore(t), reg)
	b.Client = f.srv.Client()
	return f, b
}

// Starting a sign-in costs the gateway nothing it keeps, so however many requests anyone
// sends, a person's sign-in in progress still completes.
func TestRequestsFromAnyoneCannotPushOutASignIn(t *testing.T) {
	f, b := oidcBrowser(t)
	resp := login(t, b, f, func(_ url.Values, state *http.Cookie) *http.Cookie {
		for range 5000 {
			b.StartLogin(httptest.NewRecorder(), get("/app/login"))
		}
		return state
	})
	if cookieNamed(resp, SessionCookie) == nil {
		t.Errorf("the sign-in did not complete: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// All that flood left nothing behind.
func TestStartingASignInKeepsNothing(t *testing.T) {
	_, b := oidcBrowser(t)
	for range 1000 {
		b.StartLogin(httptest.NewRecorder(), get("/app/login"))
	}
	if len(b.spent) != 0 {
		t.Errorf("%d states kept", len(b.spent))
	}
}

// A state says who made it, so it can't be written by hand, moved to another provider or
// extended, and the secrets that come from it differ each time.
func TestLoginStatesCannotBeForged(t *testing.T) {
	_, b := oidcBrowser(t)
	const id = "idp"
	soon := time.Now().Add(time.Minute)
	good := b.newLoginState(id, soon)
	if _, ok := b.loginStateOf(good); !ok {
		t.Fatal("a good state was refused")
	}
	raw, err := base64.RawURLEncoding.DecodeString(good)
	if err != nil {
		t.Fatal(err)
	}
	extended := append([]byte{}, raw...)
	binary.BigEndian.PutUint64(extended[loginStateRandom:], uint64(time.Now().Add(24*time.Hour).Unix()))
	flipped := append([]byte{}, raw...)
	flipped[len(flipped)-1] ^= 1
	short := raw[:len(raw)-1]
	_, other := oidcBrowser(t)
	for name, v := range map[string]string{
		"extended":         base64.RawURLEncoding.EncodeToString(extended),
		"a changed MAC":    base64.RawURLEncoding.EncodeToString(flipped),
		"too short":        base64.RawURLEncoding.EncodeToString(short),
		"not base64":       "!!!!",
		"empty":            "",
		"a random token":   RandomToken(64),
		"another process":  other.newLoginState(id, soon),
		"another provider": b.newLoginState("gone", soon),
	} {
		if _, ok := b.loginStateOf(v); ok {
			t.Errorf("%s was accepted", name)
		}
	}
	// A provider still configured gets its own state back, and no one else's.
	got, _ := b.loginStateOf(good)
	again, _ := b.loginStateOf(good)
	if got.registration != id || got.verifier == "" || got.nonce == "" || got.verifier == got.nonce || got != again {
		t.Errorf("%+v", got)
	}
	if !got.expires.Equal(time.Unix(soon.Unix(), 0)) {
		t.Errorf("expires %v, want %v", got.expires, soon)
	}
	second, _ := b.loginStateOf(b.newLoginState(id, soon))
	if second.verifier == got.verifier || second.nonce == got.nonce {
		t.Error("two sign-ins share a secret")
	}
	// The verifier is at least what PKCE asks for.
	if n := len(got.verifier); n < 43 || n > 128 {
		t.Errorf("verifier of %d characters", n)
	}
}

// The same callback, with a state that has not yet expired, signs in: only age differs.
func TestALoginStateThatHasNotExpiredWorks(t *testing.T) {
	f, b := oidcBrowser(t)
	resp := login(t, b, f, func(q url.Values, s *http.Cookie) *http.Cookie {
		later := b.newLoginState("idp", time.Now().Add(time.Second*30))
		st, _ := b.loginStateOf(later)
		f.challenge, f.nonce = s256(st.verifier), st.nonce
		q.Set("state", later)
		return &http.Cookie{Name: s.Name, Value: later}
	})
	if cookieNamed(resp, SessionCookie) == nil {
		t.Errorf("%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// A captured callback, cookie included, can't sign in again while the state is still valid.
// Anyone failing callbacks in between changes nothing: only a completed sign-in is
// remembered, so that record can't be pushed out by requests that sign nobody in.
func TestACapturedCallbackCannotBeReplayed(t *testing.T) {
	f, b := oidcBrowser(t)
	var back url.Values
	var state *http.Cookie
	first := login(t, b, f, func(q url.Values, s *http.Cookie) *http.Cookie {
		back, state = q, s
		return s
	})
	if cookieNamed(first, SessionCookie) == nil {
		t.Fatalf("the first callback: %d", first.StatusCode)
	}
	replay := func(name string) {
		t.Helper()
		w := httptest.NewRecorder()
		b.CompleteLogin(w, get(OIDCCallbackPath+"?"+back.Encode(), state))
		if cookieNamed(w.Result(), SessionCookie) != nil {
			t.Errorf("%s: the callback signed someone in again", name)
		}
	}
	// The fake provider takes the same code again, so what refuses this is the record.
	replay("straight away")
	for range 5000 {
		s := b.newLoginState("idp", time.Now().Add(time.Minute))
		q := url.Values{"state": {s}, "code": {"bad"}}
		b.CompleteLogin(httptest.NewRecorder(), get(OIDCCallbackPath+"?"+q.Encode(), &http.Cookie{Name: loginStateCookie, Value: s}))
	}
	if len(b.spent) != 1 {
		t.Errorf("%d states remembered after failed callbacks", len(b.spent))
	}
	replay("after 5000 failed callbacks")
}

// Two callbacks at once with the same state: one goes ahead.
func TestACallbackCannotBeRacedWithItself(t *testing.T) {
	_, b := oidcBrowser(t)
	soon := time.Now().Add(time.Minute)
	var wins atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.spendLoginState("same", soon) {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("%d went ahead", wins.Load())
	}
	// A failed one gives it back.
	b.unspendLoginState("same")
	if !b.spendLoginState("same", soon) {
		t.Error("a state given back can't be used")
	}
}

// What is remembered stays bounded, and an expired one goes first.
func TestSpentStatesAreBounded(t *testing.T) {
	_, b := oidcBrowser(t)
	soon := time.Now().Add(time.Minute)
	for i := range maxSpentStates + 500 {
		b.spendLoginState(fmt.Sprint("s", i), soon)
	}
	if len(b.spent) > maxSpentStates {
		t.Errorf("%d kept", len(b.spent))
	}
	_, c := oidcBrowser(t)
	c.spendLoginState("old", time.Now().Add(-time.Second))
	c.spendLoginState("new", soon)
	if _, kept := c.spent["old"]; kept {
		t.Error("an expired state was kept")
	}
	if _, kept := c.spent["new"]; !kept {
		t.Error("the new state was not kept")
	}
}

// Seeing the state is not enough: the verifier and the nonce take a key that never leaves the
// process. Neither is in the state, the cookie or, for the verifier, the redirect. A gateway
// with another key, such as a restored root's, accepts none of this process's states.
func TestSecretsComeFromTheStateAndAKey(t *testing.T) {
	_, b := oidcBrowser(t)
	_, other := oidcBrowser(t)
	state := b.newLoginState("idp", time.Now().Add(time.Minute))
	mine, ok := b.loginStateOf(state)
	if !ok {
		t.Fatal("refused")
	}
	if _, ok := other.loginStateOf(state); ok {
		t.Error("another key accepted this state")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(state)
	body := raw[:loginStateRandom+8]
	// The same state, signed by a gateway with another key, gives other secrets.
	theirs, ok := other.loginStateOf(base64.RawURLEncoding.EncodeToString(append(append([]byte{}, body...), other.loginStateMAC("state", "idp", body)...)))
	if !ok {
		t.Fatal("refused its own signature")
	}
	if theirs.verifier == mine.verifier || theirs.nonce == mine.nonce {
		t.Error("the secrets don't depend on the key")
	}
	for _, secret := range []string{mine.verifier, mine.nonce} {
		if strings.Contains(state, secret) {
			t.Error("the state holds a secret")
		}
	}
	w := httptest.NewRecorder()
	b.StartLogin(w, get("/app/login"))
	resp := w.Result()
	loc := resp.Header.Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := b.loginStateOf(u.Query().Get("state"))
	if !ok {
		t.Fatal("the redirect's state was refused")
	}
	if strings.Contains(loc, st.verifier) || u.Query().Get("code_challenge") != s256(st.verifier) || u.Query().Get("nonce") != st.nonce {
		t.Errorf("redirect %s", loc)
	}
	if c := cookieNamed(resp, loginStateCookie); c == nil || c.Value != u.Query().Get("state") || !c.HttpOnly || !c.Secure {
		t.Errorf("cookie %+v", c)
	}
}

// A state is bound to the browser it was started in: finishing elsewhere fails, with another
// valid state in the cookie or with none. That is login CSRF's way in, and it stays shut.
func TestAStateOnlyFinishesInItsOwnBrowser(t *testing.T) {
	f, b := oidcBrowser(t)
	for name, swap := range map[string]func(*http.Cookie) *http.Cookie{
		"no cookie": func(*http.Cookie) *http.Cookie { return &http.Cookie{Name: "other", Value: strings.Repeat("o", 43)} },
		"another sign-in's cookie": func(s *http.Cookie) *http.Cookie {
			return &http.Cookie{Name: s.Name, Value: b.newLoginState("idp", time.Now().Add(time.Minute))}
		},
	} {
		resp := login(t, b, f, func(_ url.Values, s *http.Cookie) *http.Cookie { return swap(s) })
		if cookieNamed(resp, SessionCookie) != nil {
			t.Errorf("%s: signed in", name)
		}
	}
}
