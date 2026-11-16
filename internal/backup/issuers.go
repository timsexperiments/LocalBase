package backup

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"sort"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// A binding gives a person a role, and it names the sign-in provider that vouches for them. So
// what a backup holds decides who gets which role, and it is reported per provider.

// IssuerLine is what one sign-in provider's bindings and invitations cover.
type IssuerLine struct {
	Issuer      string `json:"issuer"`
	Bindings    int    `json:"bindings"`
	Invitations int    `json:"invitations"`
	// Active says the provider is configured. Otherwise these grant nothing until it is added back.
	Active bool `json:"active"`
}

// checkBindingRows refuses what the gateway's own loader doesn't look at. LoadPolicy ignores a row
// with nothing to attach to, so a crafted one would sit unseen, and bind when a matching binding
// was added later.
func checkBindingRows(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT kind, role_id, value, issuer FROM go_binding_issuers`)
	if err != nil {
		return refuse("The access bindings in this backup can't be read: %v", err)
	}
	type row struct{ kind, role, value, issuer string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.kind, &r.role, &r.value, &r.issuer); err != nil {
			rows.Close()
			return refuse("The access bindings in this backup can't be read: %v", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return refuse("The access bindings in this backup can't be read: %v", err)
	}
	for _, r := range all {
		var query string
		switch r.kind {
		case "email":
			query = `SELECT COUNT(*) FROM auth_email_role_bindings WHERE role_id = ? AND email = ?`
		case "email-domain":
			query = `SELECT COUNT(*) FROM auth_domain_role_bindings WHERE role_id = ? AND domain = ?`
		default:
			return refuse("A binding in this backup has an unknown kind: %q.", r.kind)
		}
		if !auth.ValidIssuer(r.issuer) {
			return refuse("A binding in this backup names something that isn't an issuer: %q.", r.issuer)
		}
		var n int
		if err := db.QueryRowContext(ctx, query, r.role, r.value).Scan(&n); err != nil {
			return refuse("The access bindings in this backup can't be read: %v", err)
		}
		if n == 0 {
			return refuse("A binding's provider in this backup belongs to no binding: %s %q.", r.kind, r.value)
		}
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_roles WHERE id = ?`, r.role).Scan(&n); err != nil {
			return refuse("The access bindings in this backup can't be read: %v", err)
		}
		if n == 0 {
			return refuse("A binding's provider in this backup belongs to a role that doesn't exist: %q.", r.role)
		}
	}
	invites, err := db.QueryContext(ctx, `SELECT i.user_id, i.issuer, (SELECT COUNT(*) FROM auth_users u WHERE u.id = i.user_id) FROM go_invitation_issuers i`)
	if err != nil {
		return refuse("The invitations in this backup can't be read: %v", err)
	}
	defer invites.Close()
	for invites.Next() {
		var user, issuer string
		var n int
		if err := invites.Scan(&user, &issuer, &n); err != nil {
			return refuse("The invitations in this backup can't be read: %v", err)
		}
		if n == 0 {
			return refuse("An invitation in this backup is for a user who doesn't exist: %q.", user)
		}
		if !auth.ValidIssuer(issuer) {
			return refuse("An invitation in this backup names something that isn't an issuer: %q.", issuer)
		}
	}
	return invites.Err()
}

// bindingsOf counts, per issuer, what a database's bindings and invitations cover.
func bindingsOf(ctx context.Context, db *sql.DB, access *auth.BrowserAccess) ([]IssuerLine, auth.UnboundCount, error) {
	var none auth.UnboundCount
	active := access.Issuers()
	lines := map[string]*IssuerLine{}
	line := func(issuer string) *IssuerLine {
		if lines[issuer] == nil {
			lines[issuer] = &IssuerLine{Issuer: issuer, Active: slices.Contains(active, issuer)}
		}
		return lines[issuer]
	}
	count := func(query string, add func(*IssuerLine, int)) error {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var issuer string
			var n int
			if err := rows.Scan(&issuer, &n); err != nil {
				return err
			}
			add(line(issuer), n)
		}
		return rows.Err()
	}
	addBindings := func(l *IssuerLine, n int) { l.Bindings += n }
	for _, query := range []string{
		`SELECT issuer, COUNT(*) FROM go_binding_issuers GROUP BY issuer`,
		`SELECT issuer, COUNT(*) FROM auth_subject_role_bindings GROUP BY issuer`,
	} {
		if err := count(query, addBindings); err != nil {
			return nil, none, err
		}
	}
	if err := count(`SELECT i.issuer, COUNT(*) FROM go_invitation_issuers i JOIN auth_users u ON u.id = i.user_id WHERE u.status = 'pending' GROUP BY i.issuer`,
		func(l *IssuerLine, n int) { l.Invitations += n }); err != nil {
		return nil, none, err
	}
	out := make([]IssuerLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Issuer < out[j].Issuer })
	unbound, err := auth.CountUnbound(ctx, db)
	if err != nil {
		return nil, none, err
	}
	return out, unbound, nil
}

// describeBindings is bindingsOf for what a backup is about to hold, for its manifest.
func describeBindings(ctx context.Context, content string, o options) ([]IssuerLine, auth.UnboundCount, error) {
	var none auth.UnboundCount
	db, err := openReadOnly(filepath.Join(content, "local-base.db"), o)
	if err != nil {
		return nil, none, err
	}
	defer db.Close()
	// A database from before bindings named a provider has no such tables yet.
	for _, table := range []string{"go_binding_issuers", "go_invitation_issuers"} {
		present, err := tableExists(ctx, db, table)
		if err != nil || !present {
			return nil, none, err
		}
	}
	access, err := auth.LoadAccess(content)
	if err != nil {
		return nil, none, err
	}
	return bindingsOf(ctx, db, access)
}

// unboundNote says what will happen to rows that name no provider. They were made before bindings
// named one, and what becomes of them depends on how many providers there are at the next start.
func unboundNote(u auth.UnboundCount, access *auth.BrowserAccess) string {
	n := u.Total()
	if n == 0 {
		return ""
	}
	issuers := access.Issuers()
	switch len(issuers) {
	case 0:
		return fmt.Sprintf("%d email bindings and invitations name no sign-in provider. They grant nothing now, and whichever provider is added next takes them at the following start.", n)
	case 1:
		return fmt.Sprintf("%d email bindings and invitations name no sign-in provider. They adopt the only one, %s, at the next start.", n, issuers[0])
	}
	return fmt.Sprintf("%d email bindings and invitations name no sign-in provider, and grant nothing until you choose one: local-base access bindings adopt --issuer <issuer>", n)
}

// bindingKeys lists a database's role bindings by role name, since a role's id doesn't survive a
// policy being applied, along with the issuer each trusts.
func bindingKeys(ctx context.Context, db *sql.DB) (map[string]string, error) {
	out := map[string]string{}
	withIssuers, err := tableExists(ctx, db, "go_binding_issuers")
	if err != nil {
		return nil, err
	}
	read := func(query, kind string) error {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var role, value, issuer string
			if err := rows.Scan(&role, &value, &issuer); err != nil {
				return err
			}
			out[kind+"\x00"+role+"\x00"+value+"\x00"+issuer] = issuer
		}
		return rows.Err()
	}
	emailIssuer, domainIssuer := "''", "''"
	emailJoin, domainJoin := "", ""
	if withIssuers {
		emailIssuer, domainIssuer = "COALESCE(i.issuer, '')", "COALESCE(i.issuer, '')"
		emailJoin = ` LEFT JOIN go_binding_issuers i ON i.kind = 'email' AND i.role_id = b.role_id AND i.value = b.email`
		domainJoin = ` LEFT JOIN go_binding_issuers i ON i.kind = 'email-domain' AND i.role_id = b.role_id AND i.value = b.domain`
	}
	for _, q := range []struct{ query, kind string }{
		{`SELECT r.name, b.email, ` + emailIssuer + ` FROM auth_email_role_bindings b JOIN auth_roles r ON r.id = b.role_id` + emailJoin, "email"},
		{`SELECT r.name, b.domain, ` + domainIssuer + ` FROM auth_domain_role_bindings b JOIN auth_roles r ON r.id = b.role_id` + domainJoin, "email-domain"},
		{`SELECT r.name, b.subject, b.issuer FROM auth_subject_role_bindings b JOIN auth_roles r ON r.id = b.role_id`, "subject"},
	} {
		if err := read(q.query, q.kind); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// returningBindings counts, per issuer, the bindings a backup holds and the target no longer does.
// Nothing records that a binding was withdrawn, so it can only be reported, not kept withdrawn.
func returningBindings(ctx context.Context, staged, target *sql.DB) ([]IssuerLine, error) {
	present, err := tableExists(ctx, target, "auth_email_role_bindings")
	if err != nil || !present {
		return nil, err
	}
	backed, err := bindingKeys(ctx, staged)
	if err != nil {
		return nil, err
	}
	now, err := bindingKeys(ctx, target)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for key, issuer := range backed {
		if _, here := now[key]; !here {
			counts[issuer]++
		}
	}
	out := make([]IssuerLine, 0, len(counts))
	for issuer, n := range counts {
		if issuer == "" {
			issuer = "(naming no provider)"
		}
		out = append(out, IssuerLine{Issuer: issuer, Bindings: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Issuer < out[j].Issuer })
	return out, nil
}
