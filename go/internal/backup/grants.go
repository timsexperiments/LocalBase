package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
)

// Grant is one role: what it gives, and who has it. A count of bindings wouldn't show a file adding
// someone to a strong role, so this names them.
type Grant struct {
	Role string `json:"role"`
	// Gives is the role's permissions, set against what this root's role of that name gives.
	Gives Line `json:"gives"`
	// Who is everyone the role reaches, those new to this root first.
	Who []Line `json:"who"`
}

// grantsChangedCaution prints last, with the one about who can sign in.
const grantsChangedCaution = `This changes who has a role on this root. See "What each role gives" above.`

// readGrants reads every role and who has it, from any database. A person counts only while they're
// active or invited, since a role does nothing for anyone else. The text of each line is what's
// compared, so the same person reached two ways shows as two lines, and the same way twice as one.
func readGrants(ctx context.Context, db *sql.DB) ([]Grant, error) {
	type role struct {
		name  string
		perms []string
		who   map[string]bool
	}
	roles := map[string]*role{}
	each := func(query string, f func(values []string)) error {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }() // read-only, and rows.Err below has the answer
		columns, err := rows.Columns()
		if err != nil {
			return err
		}
		values := make([]sql.NullString, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		for rows.Next() {
			if err := rows.Scan(targets...); err != nil {
				return err
			}
			text := make([]string, len(values))
			for i, v := range values {
				text[i] = v.String
			}
			f(text)
		}
		return rows.Err()
	}
	err := each(`SELECT id, name FROM auth_roles`, func(v []string) {
		roles[v[0]] = &role{name: v[1], who: map[string]bool{}}
	})
	if err != nil {
		return nil, err
	}
	if err := each(`SELECT role_id, permission FROM auth_role_permissions`, func(v []string) {
		if r := roles[v[0]]; r != nil {
			r.perms = append(r.perms, v[1])
		}
	}); err != nil {
		return nil, err
	}
	add := func(roleID, line string) {
		if r := roles[roleID]; r != nil {
			r.who[line] = true
		}
	}
	through := func(issuer string) string {
		if issuer == "" {
			return ", once a sign-in provider is chosen"
		}
		return ", signing in through " + issuer
	}
	for _, q := range []struct {
		query string
		line  func(v []string) string
	}{
		{`SELECT role_id, subject, issuer FROM auth_subject_role_bindings`,
			func(v []string) string { return v[1] + " at " + v[2] }},
		{`SELECT b.role_id, b.email, i.issuer FROM auth_email_role_bindings b
			LEFT JOIN go_binding_issuers i ON i.kind = 'email' AND i.role_id = b.role_id AND i.value = b.email`,
			func(v []string) string { return v[1] + through(v[2]) }},
		{`SELECT b.role_id, b.domain, i.issuer FROM auth_domain_role_bindings b
			LEFT JOIN go_binding_issuers i ON i.kind = 'email-domain' AND i.role_id = b.role_id AND i.value = b.domain`,
			func(v []string) string { return "anyone at " + v[1] + through(v[2]) }},
		{`SELECT ur.role_id, a.subject, a.issuer FROM auth_user_roles ur JOIN auth_users u ON u.id = ur.user_id
			JOIN auth_identities a ON a.user_id = u.id WHERE u.status IN ('active', 'pending')`,
			func(v []string) string { return v[1] + " at " + v[2] }},
		{`SELECT ur.role_id, e.email, i.issuer FROM auth_user_roles ur JOIN auth_users u ON u.id = ur.user_id
			JOIN auth_user_emails e ON e.user_id = u.id LEFT JOIN go_invitation_issuers i ON i.user_id = u.id
			WHERE u.status IN ('active', 'pending') AND NOT EXISTS (SELECT 1 FROM auth_identities a WHERE a.user_id = u.id)`,
			func(v []string) string { return v[1] + ", invited" + through(v[2]) }},
	} {
		if err := each(q.query, func(v []string) { add(v[0], q.line(v)) }); err != nil {
			return nil, err
		}
	}
	out := make([]Grant, 0, len(roles))
	for _, r := range roles {
		g := Grant{Role: r.name, Gives: Line{Value: joined(r.perms)}, Who: []Line{}}
		for line := range r.who {
			g.Who = append(g.Who, Line{Value: line})
		}
		slices.SortFunc(g.Who, func(a, b Line) int { return strings.Compare(a.Value, b.Value) })
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b Grant) int { return strings.Compare(a.Role, b.Role) })
	return out, nil
}

// liveGrants reads the root being replaced, and only reads.
func liveGrants(ctx context.Context, target string) ([]Grant, error) {
	db, err := openReadOnly(filepath.Join(target, "local-base.db"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }() // read-only, so there's nothing a failed close could lose
	return readGrants(ctx, db)
}

// compareGrants sets grants against what the root being replaced has, and says whether any differ. A
// role or a person new to the root goes first in its list, so that nothing can bury them.
func compareGrants(grants []Grant, live []Grant, liveErr error) bool {
	changed := false
	for i := range grants {
		g := &grants[i]
		if liveErr != nil {
			g.Gives.Against, changed = AgainstUnreadable, true
			continue
		}
		j := slices.IndexFunc(live, func(l Grant) bool { return l.Role == g.Role })
		if j < 0 {
			g.Gives.Against, changed = AgainstNew, true
			for k := range g.Who {
				g.Who[k].Against = AgainstNew
			}
			continue
		}
		if live[j].Gives.Value != g.Gives.Value {
			g.Gives.Against, g.Gives.ThisRoot, changed = AgainstDiffers, live[j].Gives.Value, true
			if g.Gives.ThisRoot == "" {
				g.Gives.ThisRoot = "nothing"
			}
		}
		for k := range g.Who {
			known := slices.ContainsFunc(live[j].Who, func(l Line) bool { return l.Value == g.Who[k].Value })
			if !known {
				g.Who[k].Against, changed = AgainstNew, true
			}
		}
		slices.SortStableFunc(g.Who, func(a, b Line) int {
			switch {
			case a.Against != "" && b.Against == "":
				return -1
			case a.Against == "" && b.Against != "":
				return 1
			}
			return 0
		})
	}
	return changed
}
