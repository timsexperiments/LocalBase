package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Only /app/login?switch=1 asks the provider for its account chooser.
func TestLoginSwitchAccount(t *testing.T) {
	b, _ := oidcSwitchFixture(t)
	prompt := func(target string) (string, bool) {
		t.Helper()
		w := httptest.NewRecorder()
		b.StartLogin(w, get(target))
		if w.Code != 303 {
			t.Fatalf("%s: %d %s", target, w.Code, w.Body)
		}
		loc, err := url.Parse(w.Header().Get("Location"))
		if err != nil {
			t.Fatal(err)
		}
		_, has := loc.Query()["prompt"]
		return loc.Query().Get("prompt"), has
	}
	if got, has := prompt("/app/login"); has {
		t.Errorf("a normal sign-in asked for %q", got)
	}
	if got, _ := prompt("/app/login?switch=1"); got != "select_account" {
		t.Errorf("switch=1: %q", got)
	}
	for _, other := range []string{"/app/login?switch=0", "/app/login?switch=true", "/app/login?switch="} {
		if got, has := prompt(other); has {
			t.Errorf("%s asked for %q", other, got)
		}
	}
	// The value is fixed: nothing from the request reaches the provider's prompt.
	if got, _ := prompt("/app/login?switch=1&prompt=none"); got != "select_account" {
		t.Errorf("a request's own prompt: %q", got)
	}
	// With several providers, it carries through the chosen one.
	if got, _ := prompt("/app/login?provider=idp&switch=1"); got != "select_account" {
		t.Errorf("with a provider: %q", got)
	}
}

// With several providers the chooser page comes first, and the switch survives it.
func TestLoginSwitchThroughChooser(t *testing.T) {
	f := newFakeIdP(t)
	reg := func(id string) Registration {
		return Registration{Kind: "oidc", ID: id, Name: id, Issuer: f.srv.URL, ClientID: "cid", ClientAuthentication: &ClientAuth{Kind: "client-secret-basic", ClientSecret: "shh"}}
	}
	b := browserFor(t, testStore(t), reg("one"), reg("two"))
	b.Client = f.srv.Client()
	page := func(target string) string {
		w := httptest.NewRecorder()
		b.StartLogin(w, get(target))
		return w.Body.String()
	}
	if got := page("/app/login?switch=1"); !strings.Contains(got, `/app/login?provider=one&amp;switch=1`) && !strings.Contains(got, `/app/login?provider=one&switch=1`) {
		t.Errorf("the chooser dropped the switch: %s", got)
	}
	if got := page("/app/login"); strings.Contains(got, "switch") {
		t.Errorf("a normal chooser: %s", got)
	}
}

func oidcSwitchFixture(t *testing.T) (*Browser, *fakeIdP) {
	t.Helper()
	f := newFakeIdP(t)
	b := browserFor(t, testStore(t), Registration{Kind: "oidc", ID: "idp", Name: "IdP", Issuer: f.srv.URL, ClientID: "cid",
		ClientAuthentication: &ClientAuth{Kind: "client-secret-basic", ClientSecret: "shh"}})
	b.Client = f.srv.Client()
	return b, f
}

// Asking for the account chooser changes nothing else about the sign-in: the same
// parameters and fresh state, nonce and challenge, the callback signs in, and the state
// still works only once.
func TestLoginSwitchKeepsTheSignInFlow(t *testing.T) {
	b, f := oidcSwitchFixture(t)
	start := func(target string) (*http.Response, url.Values) {
		t.Helper()
		w := httptest.NewRecorder()
		b.StartLogin(w, get(target))
		resp := w.Result()
		loc, err := url.Parse(resp.Header.Get("Location"))
		if err != nil || resp.StatusCode != 303 {
			t.Fatalf("%s: %d %v", target, resp.StatusCode, err)
		}
		return resp, loc.Query()
	}
	_, plain := start("/app/login")
	resp, switched := start("/app/login?switch=1")
	for k := range plain {
		if _, ok := switched[k]; !ok {
			t.Errorf("switch=1 lost %q", k)
		}
	}
	for k := range switched {
		if _, ok := plain[k]; !ok && k != "prompt" {
			t.Errorf("switch=1 added %q", k)
		}
	}
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if switched.Get(k) == "" || switched.Get(k) == plain.Get(k) {
			t.Errorf("%s is not fresh: %q", k, switched.Get(k))
		}
	}
	f.challenge, f.nonce = switched.Get("code_challenge"), switched.Get("nonce")
	state := cookieNamed(resp, loginStateCookie)
	if state == nil || state.Value != switched.Get("state") {
		t.Fatalf("state cookie %+v", state)
	}
	back := url.Values{"state": {switched.Get("state")}, "code": {"good-code"}}
	w := httptest.NewRecorder()
	b.CompleteLogin(w, get(OIDCCallbackPath+"?"+back.Encode(), state))
	if cookieNamed(w.Result(), SessionCookie) == nil {
		t.Fatalf("the callback signed nobody in: %d", w.Code)
	}
	w = httptest.NewRecorder()
	b.CompleteLogin(w, get(OIDCCallbackPath+"?"+back.Encode(), state))
	if cookieNamed(w.Result(), SessionCookie) != nil {
		t.Error("the state worked twice")
	}
}
