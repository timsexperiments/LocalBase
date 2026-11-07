package auth

import (
	"bytes"
	"io"
	"log/slog"
	"testing"
	"time"
)

// What NewBrowser sets up, part by part, so that laying it out differently can't change any of it.
func TestNewBrowserSetsUpWhatSignInNeeds(t *testing.T) {
	st := testStore(t)
	root := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &BrowserAccess{Origin: testOrigin, Provider: Provider{Kind: "direct", Registrations: []Registration{
		{Kind: "oidc", ID: "idp", Issuer: "https://idp.example.com", ClientID: "client"},
		{Kind: "magic-link", ID: "mail"},
	}}}
	for _, c := range []*BrowserAccess{cfg, nil} {
		b := NewBrowser(st, root, c, log)
		if b.Store != st || b.Root != root || b.Config != c || b.Log != log {
			t.Errorf("config %v: the parts it was given weren't kept", c != nil)
		}
		// An identity provider gets 5 seconds, and no redirect is followed.
		if b.Client == nil || b.Client.Timeout != 5*time.Second {
			t.Errorf("config %v: the client: %+v", c != nil, b.Client)
		} else if b.Client.CheckRedirect == nil || b.Client.CheckRedirect(nil, nil) == nil {
			t.Errorf("config %v: the client would follow a redirect", c != nil)
		}
		if b.spent == nil {
			t.Errorf("config %v: nowhere to remember spent sign-in states", c != nil)
		}
		if len(b.stateKey) != 32 || bytes.Equal(b.stateKey, make([]byte, 32)) {
			t.Errorf("config %v: the state key isn't 32 random bytes", c != nil)
		}
		if cap(b.magicWork) != magicLinkWorkers {
			t.Errorf("config %v: %d magic-link workers, want %d", c != nil, cap(b.magicWork), magicLinkWorkers)
		}
		if b.failedLog == nil || b.failedLog.Every != time.Minute || b.failedLog.Burst != 20 {
			t.Errorf("config %v: failed sign-ins are logged by %+v", c != nil, b.failedLog)
		}
		if b.cfJWKS != nil {
			t.Errorf("config %v: a Cloudflare key set without Cloudflare", c != nil)
		}
		if b.oidc == nil {
			t.Errorf("config %v: no map for OpenID Connect", c != nil)
		}
	}
	// One OpenID Connect runtime per OIDC registration, and none for a magic link.
	b := NewBrowser(st, root, cfg, log)
	if len(b.oidc) != 1 || b.oidc["idp"] == nil || b.oidc["idp"].reg.ID != "idp" {
		t.Errorf("OpenID Connect runtimes: %v", b.oidc)
	}
	// Each browser makes its own key.
	if other := NewBrowser(st, root, cfg, log); bytes.Equal(other.stateKey, b.stateKey) {
		t.Error("two browsers share a state key")
	}
}
