package auth

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// A state works once even after others have come and gone, and what is remembered stays small.
func TestSpentStatesAreRememberedAndBounded(t *testing.T) {
	_, b := oidcBrowser(t)
	soon := time.Now().Add(time.Minute)
	if !b.spendLoginState("first", soon) || b.spendLoginState("first", soon) {
		t.Error("a state must work exactly once")
	}
	for i := range 5000 {
		b.spendLoginState(fmt.Sprint("s", i), soon)
	}
	if len(b.spent) > maxSpentStates {
		t.Errorf("%d kept", len(b.spent))
	}
	// One that has expired is forgotten first, without pushing out anything else.
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
