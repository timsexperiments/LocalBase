package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// How a line compares with the root being replaced. Empty means it's the same, or there's nothing to
// compare it with.
const (
	AgainstDiffers    = "differs"    // the root has something else, in ThisRoot
	AgainstNew        = "new"        // the root has none
	AgainstUnreadable = "unreadable" // the root's can't be read, which says nothing about what it has
	AgainstGone       = "gone"       // the root has it and the backup doesn't
)

// Line is one thing a restore would decide, and what the root being replaced has instead. Value comes
// first because it's the fact. Named is what the backup calls it, which is only the file's word.
type Line struct {
	Value    string `json:"value"`
	Named    string `json:"named,omitempty"`
	ThisRoot string `json:"thisRoot,omitempty"`
	Against  string `json:"against,omitempty"`
}

// Text is the line as it reads in a report. has is the word for what the root does with its own value.
func (l Line) Text(has string) string {
	var notes []string
	if l.Named != "" {
		notes = append(notes, "this file names it "+l.Named)
	}
	switch l.Against {
	case AgainstDiffers:
		notes = append(notes, "this root "+has+" "+l.ThisRoot)
	case AgainstNew:
		notes = append(notes, "new to this root")
	case AgainstUnreadable:
		notes = append(notes, "this root's can't be read")
	}
	if len(notes) == 0 {
		return l.Value
	}
	return l.Value + " (" + strings.Join(notes, "; ") + ")"
}

// SignIn says who could sign in after a restore, and what they would get, in the values that decide
// it. It holds no client id, username, password or header value.
type SignIn struct {
	Who []Line `json:"who"`
	// Goes is what the root being replaced lets people sign in with, and the backup doesn't.
	Goes []Line `json:"goes,omitempty"`
	// Everyone is what anyone who signs in gets, with no role. An empty value means nothing.
	Everyone Line `json:"everyone"`
	// IfPolicyRemoved is what the file would give everyone, which a policy holds back for now.
	IfPolicyRemoved string `json:"ifPolicyRemoved,omitempty"`
	Origin          *Line  `json:"origin,omitempty"`
	Mail            *Line  `json:"mail,omitempty"`
}

// signInChangedCaution prints last, just before the question, where it can't be missed in a long report.
const signInChangedCaution = `This changes who can sign in on this root. See "Who can sign in" above.`

// signInUnknownCaution is for when nothing differs that can be read, and something can't be.
const signInUnknownCaution = `This may change who can sign in on this root, since part of it can't be read. See "Who can sign in" above.`

// signInValues is what a root's files and database say. A failed read is kept beside the value,
// so that it's never taken for having none.
type signInValues struct {
	ways                []Line
	everyone, ifRemoved []string
	origin, mail        string
	accessErr, mailErr  error
	policyErr           error
}

func wayOf(r auth.Registration) Line {
	named := r.Kind + ":" + r.ID
	switch r.Kind {
	case "oidc":
		return Line{Value: r.Issuer, Named: named}
	case "github-oauth":
		return Line{Value: "GitHub accounts", Named: named}
	case "magic-link":
		return Line{Value: "Invited people, with a link sent by email", Named: named}
	}
	return Line{Value: "A sign-in provider of a kind this version doesn't name", Named: named}
}

// readSignIn reads what decides who can sign in. Each read fails on its own. With no database, the
// caller has its own reason why and sets policyErr.
func readSignIn(ctx context.Context, dir string, db *sql.DB) signInValues {
	var v signInValues
	var access *auth.BrowserAccess
	access, v.accessErr = auth.LoadAccess(dir)
	if access != nil {
		v.origin = access.Origin
		if access.Provider.Kind == "cloudflare-access" {
			v.ways = append(v.ways, Line{Value: fmt.Sprintf("Cloudflare Access team %s, audience %s", access.Provider.TeamDomain, access.Provider.Audience),
				Named: access.Provider.Kind})
		}
		for _, r := range access.Provider.Registrations {
			v.ways = append(v.ways, wayOf(r))
		}
	}
	email, err := auth.LoadEmail(dir)
	v.mailErr = err
	if email != nil {
		v.mail = fmt.Sprintf("%s:%d", email.Host, email.Port)
	}
	var policy bool
	var fromPolicy []string
	if db != nil {
		policy, fromPolicy, v.policyErr = readEveryone(ctx, db)
	}
	switch {
	case !policy && access != nil:
		v.everyone = access.Permissions
	case policy:
		if access != nil {
			v.ifRemoved = access.Permissions
		}
		v.everyone = fromPolicy
	}
	return v
}

// readEveryone says whether there's an access policy, and what it gives anyone who signs in. It reads
// only the tables that decide that, as auth.LoadPolicy does, since that also reads tables a root last
// opened by an older version doesn't have yet.
func readEveryone(ctx context.Context, db *sql.DB) (policy bool, everyone []string, err error) {
	present, err := tablesExist(ctx, db, "auth_settings", "auth_roles", "auth_role_permissions")
	if err != nil || !present {
		return false, nil, err
	}
	var defaultRole sql.NullString
	err = db.QueryRowContext(ctx, `SELECT default_role_id FROM auth_settings WHERE id = 'default'`).Scan(&defaultRole)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if !defaultRole.Valid {
		return true, nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT p.permission FROM auth_role_permissions p JOIN auth_roles r ON r.id = p.role_id WHERE r.id = ?`, defaultRole.String)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = rows.Close() }() // read-only, and rows.Err below has the answer
	for rows.Next() {
		var permission string
		if err := rows.Scan(&permission); err != nil {
			return false, nil, err
		}
		everyone = append(everyone, permission)
	}
	return true, auth.Canonical(everyone), rows.Err()
}

func joined(perms []string) string {
	sorted := slices.Sorted(slices.Values(perms))
	return strings.Join(slices.Compact(sorted), ", ")
}

// signInReport is what the backup says, set against what the root being replaced says. With no root
// to compare, there's nothing to set it against, and live is nil.
func signInReport(backed signInValues, live *signInValues) (*SignIn, string) {
	s := &SignIn{Who: append([]Line{}, backed.ways...), Everyone: Line{Value: joined(backed.everyone)}, IfPolicyRemoved: joined(backed.ifRemoved)}
	if backed.origin != "" {
		s.Origin = &Line{Value: backed.origin}
	}
	if backed.mail != "" {
		s.Mail = &Line{Value: backed.mail}
	}
	if live == nil {
		return s, ""
	}
	changed, unknown := false, false
	note := func(l *Line, against, thisRoot string) {
		l.Against, l.ThisRoot = against, thisRoot
		if against == AgainstUnreadable {
			unknown = true
		} else {
			changed = true
		}
	}
	for i := range s.Who {
		way := &s.Who[i]
		j := slices.IndexFunc(live.ways, func(w Line) bool { return w.Named == way.Named })
		switch {
		case live.accessErr != nil:
			note(way, AgainstUnreadable, "")
		case j < 0:
			note(way, AgainstNew, "")
		case live.ways[j].Value != way.Value:
			note(way, AgainstDiffers, live.ways[j].Value)
		}
	}
	for _, w := range live.ways {
		if !slices.ContainsFunc(s.Who, func(l Line) bool { return l.Named == w.Named }) {
			s.Goes = append(s.Goes, w)
			changed = true
		}
	}
	// Who everyone is depends on the policy, and without one on the access file.
	switch thisRoot := joined(live.everyone); {
	case live.policyErr != nil || live.accessErr != nil:
		note(&s.Everyone, AgainstUnreadable, "")
	case thisRoot == s.Everyone.Value:
	case thisRoot == "":
		note(&s.Everyone, AgainstDiffers, "nothing")
	default:
		note(&s.Everyone, AgainstDiffers, thisRoot)
	}
	compareValue := func(mine **Line, theirs string, readErr error) {
		if *mine == nil && (theirs != "" || readErr != nil) {
			*mine = &Line{Value: "none"}
		}
		switch {
		case *mine == nil:
		case readErr != nil:
			note(*mine, AgainstUnreadable, "")
		case theirs == "":
			note(*mine, AgainstNew, "")
		case theirs != (*mine).Value:
			note(*mine, AgainstDiffers, theirs)
		}
	}
	compareValue(&s.Origin, live.origin, live.accessErr)
	compareValue(&s.Mail, live.mail, live.mailErr)
	switch {
	case changed:
		return s, signInChangedCaution
	case unknown:
		return s, signInUnknownCaution
	}
	return s, ""
}

// liveSignIn reads the root being replaced. It only reads: that root may well be replaced because
// something in it is broken, and that has to show as unread, not as having none.
func liveSignIn(ctx context.Context, target string) signInValues {
	db, err := openReadOnly(filepath.Join(target, "local-base.db"))
	if err != nil {
		v := readSignIn(ctx, target, nil)
		v.policyErr = err
		return v
	}
	defer func() { _ = db.Close() }() // read-only, so there's nothing a failed close could lose
	return readSignIn(ctx, target, db)
}
