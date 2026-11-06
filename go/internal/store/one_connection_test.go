package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
)

// The pool is bounded, so a transaction that asks for a second connection while it holds one can wait
// forever once enough of them do it at once. With a pool of one, any such site waits at once, and
// runs out of its time. A site in store.TransactionSites that isn't here fails.
func TestEveryTransactionNeedsOnlyOneConnection(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.DB.SetMaxOpenConns(1)
	const (
		issuer = "https://idp.example"
		owner  = "owner@example.com"
		person = "person@example.com"
	)
	access := &auth.BrowserAccess{Origin: "https://lb.example", Permissions: []string{"models:read"},
		Provider: auth.Provider{Kind: "direct", Registrations: []auth.Registration{{Kind: "oidc", ID: "idp", Name: "IdP", Issuer: issuer, ClientID: "c", ClientAuthentication: &auth.ClientAuth{Kind: "none"}}}}}
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		t.Fatal(err)
	}
	cfg := conf.Default(t.TempDir(), 64, cat)
	rec, err := usage.New(st, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec.Add(usage.Event{Route: usage.Routes[0], Kind: "api-key", Principal: "p", At: time.Now()})
	magicIssuer := auth.MagicIssuer(access.Origin)
	var magicToken string

	// In an order where each has what it needs. migrate runs in store.Open above.
	steps := []struct {
		site string
		run  func(ctx context.Context) error
	}{
		{"auth.ApplyPolicy", func(ctx context.Context) error { return auth.ApplyPolicy(ctx, st, auth.StarterPolicy(owner, issuer)) }},
		{"auth.InviteUser", func(ctx context.Context) error {
			_, err := auth.InviteUser(ctx, st, person, []string{"admin"}, issuer)
			return err
		}},
		{"auth.evaluatePolicy", func(ctx context.Context) error {
			_, err := auth.ResolvePolicy(ctx, st, auth.Identity{Issuer: issuer, Subject: "s", VerifiedEmail: person}, true)
			return err
		}},
		{"auth.ReplaceUserRoles", func(ctx context.Context) error {
			_, err := auth.ReplaceUserRoles(ctx, st, person, []string{"admin"})
			return err
		}},
		{"auth.SetUserStatus", func(ctx context.Context) error {
			_, err := auth.SetUserStatus(ctx, st, person, "active")
			return err
		}},
		{"auth.IssueMagicLink", func(ctx context.Context) error {
			var err error
			magicToken, _, err = auth.IssueMagicLink(ctx, st, owner, magicIssuer)
			return err
		}},
		{"auth.ConsumeMagicLink", func(ctx context.Context) error {
			_, err := auth.ConsumeMagicLink(ctx, st, magicToken, magicIssuer)
			return err
		}},
		{"auth.IssueLocalLink", func(ctx context.Context) error {
			_, err := auth.IssueLocalLink(ctx, st)
			return err
		}},
		{"auth.AdoptBindingIssuers", func(ctx context.Context) error {
			_, err := auth.AdoptBindingIssuers(ctx, st, access)
			return err
		}},
		{"auth.AdoptBindingIssuer", func(ctx context.Context) error {
			_, err := auth.AdoptBindingIssuer(ctx, st, access, issuer)
			return err
		}},
		{"auth.RemoveUser", func(ctx context.Context) error {
			_, err := auth.RemoveUser(ctx, st, person)
			return err
		}},
		{"auth.ClearPolicy", func(ctx context.Context) error {
			_, err := auth.ClearPolicy(ctx, st)
			return err
		}},
		{"usage.write", func(ctx context.Context) error { return rec.Flush(ctx) }},
		{"store.SaveConfig", func(ctx context.Context) error { return st.SaveConfig(ctx, cfg) }},
		{"store.SaveConfigMarking", func(ctx context.Context) error { return st.SaveConfigMarking(ctx, cfg) }},
		{"store.MarkRestartPendingUnlessChanged", func(ctx context.Context) error { return st.MarkRestartPendingUnlessChanged(ctx, cfg) }},
		{"store.AcknowledgeStatic", func(ctx context.Context) error { return st.AcknowledgeStatic(ctx, cfg) }},
	}
	driven := map[string]bool{"store.migrate": true}
	for _, s := range steps {
		driven[s.site] = true
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := s.run(ctx)
		cancel()
		if err != nil {
			t.Errorf("%s: %v", s.site, err)
		}
	}
	for site := range store.TransactionSites() {
		if !driven[site] {
			t.Errorf("%s isn't driven here: add it, so a second connection in it is found", site)
		}
	}
}
