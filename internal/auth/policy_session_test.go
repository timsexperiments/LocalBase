package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// A sign-in whose permissions or session can't be read or stored leaves no session and no cookie for
// one, and isn't told it has no access: only that signing in failed.
func TestASignInTheDatabaseCannotFinish(t *testing.T) {
	reg := Registration{Kind: "oidc", ID: "idp", Name: "IdP", Issuer: "https://idp.example", ClientID: "c", ClientAuthentication: &ClientAuth{Kind: "none"}}
	st := testStore(t)
	b := browserFor(t, st, reg)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	b.establish(w, get(OIDCCallbackPath), Identity{Issuer: reg.Issuer, Subject: "s", VerifiedEmail: "person@example.com"}, "owner")
	if w.Code != 303 && w.Code != 302 {
		t.Errorf("status %d: %.200q", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "signin=failed") {
		t.Errorf("location %q", loc)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			t.Errorf("a cookie for a session that wasn't stored: %v", c)
		}
	}

	// A magic link: the same, whichever step can't read.
	st2 := testStore(t)
	m := browserFor(t, st2, Registration{Kind: "magic-link", ID: "email", Name: "Email"})
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", MagicCallbackPath, strings.NewReader("token="+strings.Repeat("a", 43)))
	r.Host = "lb.example.com"
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	m.CompleteMagicLink(w, r)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "signin=failed") {
		t.Errorf("magic link: %d, location %q", w.Code, loc)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie && c.Value != "" {
			t.Errorf("magic link: a cookie for a session that wasn't stored: %v", c)
		}
	}
}
