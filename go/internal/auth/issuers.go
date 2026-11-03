package auth

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// An email address proves nothing by itself: what counts is who vouches for it. Email and
// domain bindings and invitations each name the sign-in provider they trust, so a provider
// added later can't sign in as someone else's people by claiming their address (DESIGN
// §13.6, item 5b). The names live in Go's own tables, so a rollback finds its schema as
// it left it. A binding or invitation with no name matches nobody.

// Issuers are the providers that can sign someone in under this configuration, by the
// issuer their identities carry.
func (b *BrowserAccess) Issuers() []string {
	if b == nil {
		return nil
	}
	var out []string
	switch b.Provider.Kind {
	case "cloudflare-access":
		out = append(out, "https://"+b.Provider.TeamDomain)
	case "direct":
		for _, r := range b.Provider.Registrations {
			switch r.Kind {
			case "oidc":
				out = append(out, r.Issuer)
			case "magic-link":
				out = append(out, MagicIssuer(b.Origin))
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func issuerRequired(what string, issuers []string) *AccessError {
	return &AccessError{"issuer-required", what + " must name the sign-in provider that vouches for it, because several are configured. Add \"issuer\" with one of: " + joinComma(issuers) + "."}
}

func issuerUnknown(issuer string, issuers []string) *AccessError {
	known := "no provider is"
	if len(issuers) > 0 {
		known = "these are: " + joinComma(issuers)
	}
	return &AccessError{"issuer-unknown", issuer + " is not a configured sign-in provider; " + known + "."}
}

// chooseIssuer is who vouches for an address when nobody said: the provider the person
// adding their own address signed in with, or the only one there is. It never guesses
// between several. With none configured it answers "": the caller refuses that its own
// way, and a binding left without a name matches nobody.
func chooseIssuer(what, email string, issuers []string, caller *Identity) (string, error) {
	if caller != nil && email != "" && strings.EqualFold(caller.VerifiedEmail, email) && slices.Contains(issuers, caller.Issuer) {
		return caller.Issuer, nil
	}
	switch len(issuers) {
	case 0:
		return "", nil
	case 1:
		return issuers[0], nil
	}
	return "", issuerRequired(what, issuers)
}

// BindIssuers names the provider on each email and domain binding that has none.
func (p *Policy) BindIssuers(issuers []string, caller *Identity) error {
	for i := range p.Bindings {
		b := &p.Bindings[i]
		if b.Issuer != "" {
			continue // already named, as a subject binding always is
		}
		issuer, err := chooseIssuer("A binding for "+b.Email+b.Domain, b.Email, issuers, caller)
		if err != nil {
			return err
		}
		b.Issuer = issuer
	}
	return nil
}

// InviteIssuer is the provider an invitation is for: the one named, or chooseIssuer's
// answer. With no provider configured there is nothing to check a name against: the
// invitation itself is refused for that.
func InviteIssuer(named, email string, issuers []string, caller *Identity) (string, error) {
	if named == "" {
		return chooseIssuer("An invitation", email, issuers, caller)
	}
	if len(issuers) > 0 && !slices.Contains(issuers, named) {
		return "", issuerUnknown(named, issuers)
	}
	return named, nil
}

// CheckBindingIssuers refuses a new email or domain binding on a provider that isn't
// configured. One that is already there may stay: its provider may come back, and until
// it does the binding matches nobody.
func (g *Guard) CheckBindingIssuers(next *Policy) error {
	had := bindingSet(g.Policy)
	for _, b := range next.Bindings {
		if b.Kind != "subject" && b.Issuer != "" && !had[b] && !slices.Contains(g.Issuers, b.Issuer) {
			return issuerUnknown(b.Issuer, g.Issuers)
		}
	}
	return nil
}

// unbound is what says nothing yet about who vouches for it: rows written before
// addresses were tied to a provider, or by a rollback to a version that doesn't know.
type unbound struct {
	bindings [][3]string       // kind, role id, value
	open     []string          // users with no identity yet
	linked   map[string]string // users already signed in through one provider
}

func loadUnbound(ctx context.Context, q queryer) (*unbound, error) {
	u := &unbound{linked: map[string]string{}}
	for _, t := range []struct{ kind, table, column string }{
		{"email", "auth_email_role_bindings", "email"},
		{"email-domain", "auth_domain_role_bindings", "domain"},
	} {
		rows, err := q.QueryContext(ctx, fmt.Sprintf(`SELECT b.role_id, b.%[2]s FROM %[1]s b WHERE NOT EXISTS
			(SELECT 1 FROM go_binding_issuers i WHERE i.kind = ? AND i.role_id = b.role_id AND i.value = b.%[2]s)`, t.table, t.column), t.kind)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var role, value string
			if err := rows.Scan(&role, &value); err != nil {
				rows.Close()
				return nil, err
			}
			u.bindings = append(u.bindings, [3]string{t.kind, role, value})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	rows, err := q.QueryContext(ctx, `SELECT u.id, (SELECT COUNT(DISTINCT a.issuer) FROM auth_identities a WHERE a.user_id = u.id),
		(SELECT MIN(a.issuer) FROM auth_identities a WHERE a.user_id = u.id) FROM auth_users u
		WHERE u.status != 'removed' AND NOT EXISTS (SELECT 1 FROM go_invitation_issuers i WHERE i.user_id = u.id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		var issuer sql.NullString
		if err := rows.Scan(&id, &n, &issuer); err != nil {
			return nil, err
		}
		switch n {
		case 0:
			u.open = append(u.open, id)
		case 1:
			u.linked[id] = issuer.String
		}
		// Someone linked through several providers needs no name to sign in, and
		// naming one of them would be a guess.
	}
	return u, rows.Err()
}

func (u *unbound) write(ctx context.Context, q queryer, issuer string) (int, error) {
	n := 0
	for _, b := range u.bindings {
		if _, err := q.ExecContext(ctx, `INSERT INTO go_binding_issuers (kind, role_id, value, issuer) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, b[0], b[1], b[2], issuer); err != nil {
			return 0, err
		}
		n++
	}
	name := func(id, issuer string) error {
		_, err := q.ExecContext(ctx, `INSERT INTO go_invitation_issuers (user_id, issuer) VALUES (?, ?) ON CONFLICT (user_id) DO NOTHING`, id, issuer)
		return err
	}
	for _, id := range u.open {
		if err := name(id, issuer); err != nil {
			return 0, err
		}
		n++
	}
	for id, linked := range u.linked {
		if err := name(id, linked); err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// UnboundCount is what names no provider, and so grants nothing.
type UnboundCount struct{ Emails, Domains, Invitations int }

// Total is how many there are.
func (c UnboundCount) Total() int { return c.Emails + c.Domains + c.Invitations }

// Notice is what to tell someone who can act on it, or "" when there is nothing to say.
func (c UnboundCount) Notice() string {
	if c.Total() == 0 {
		return ""
	}
	return fmt.Sprintf("%d bindings and invitations are not bound to a sign-in provider, so they grant nothing. Run: local-base access bindings adopt --issuer <issuer>", c.Total())
}

// CountUnbound counts what names no provider. People linked through a provider already
// sign in, and are named at the next start without anyone choosing, so they don't count.
func CountUnbound(ctx context.Context, q queryer) (UnboundCount, error) {
	u, err := loadUnbound(ctx, q)
	if err != nil {
		return UnboundCount{}, err
	}
	c := UnboundCount{Invitations: len(u.open)}
	for _, b := range u.bindings {
		if b[0] == "email" {
			c.Emails++
		} else {
			c.Domains++
		}
	}
	return c, nil
}

// AdoptBindingIssuers names the provider on bindings and invitations that have none, and
// reports how many. With one provider there is nothing to guess. With several it never
// guesses, because the wrong answer would let one provider sign in as another's people:
// those rows keep granting nothing until someone chooses (AdoptBindingIssuer). People
// already linked through one provider need no choice either way.
func AdoptBindingIssuers(ctx context.Context, st *store.Store, cfg *BrowserAccess) (int, error) {
	issuers := cfg.Issuers()
	if len(issuers) == 0 {
		return 0, nil
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	u, err := loadUnbound(ctx, tx)
	if err != nil {
		return 0, err
	}
	if len(issuers) > 1 {
		u.bindings, u.open = nil, nil
	}
	n, err := u.write(ctx, tx, issuers[0])
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// AdoptBindingIssuer names one configured provider on everything that has none: the
// answer when several are configured.
func AdoptBindingIssuer(ctx context.Context, st *store.Store, cfg *BrowserAccess, issuer string) (int, error) {
	if !slices.Contains(cfg.Issuers(), issuer) {
		return 0, issuerUnknown(issuer, cfg.Issuers())
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	u, err := loadUnbound(ctx, tx)
	if err != nil {
		return 0, err
	}
	n, err := u.write(ctx, tx, issuer)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
