package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// comparedKey is what a restore compares of a key: everything that decides what it can do, including
// the hash of its secret. These reads are of the root being replaced, where a failure must not look
// like finding nothing, which would bring a revoked key back.
type comparedKey struct {
	name, prefix, hash, rotated string
	// scopes isn't set on a root older than key permissions, which can't say what its keys may do.
	scopes, expires, revoked sql.NullString
}

// readComparedKeys reads every key from any database, so that whatever else needs to compare keys
// reads the same facts.
func readComparedKeys(ctx context.Context, db *sql.DB) (map[string]comparedKey, error) {
	out := map[string]comparedKey{}
	present, err := tableExists(ctx, db, "api_keys")
	if err != nil || !present {
		return out, err
	}
	scopes, err := columnExists(ctx, db, "api_keys", "scopes")
	if err != nil {
		return nil, err
	}
	column := "NULL"
	if scopes {
		column = "scopes"
	}
	rows, err := db.QueryContext(ctx, `SELECT id, name, prefix, key_hash, `+column+`, last_rotated_at, expires_at, revoked_at FROM api_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var k comparedKey
		if err := rows.Scan(&id, &k.name, &k.prefix, &k.hash, &k.scopes, &k.rotated, &k.expires, &k.revoked); err != nil {
			return nil, err
		}
		out[id] = k
	}
	return out, rows.Err()
}

// differences says how a backup's copy of a key differs from the one on the root, in words.
func (k comparedKey) differences(backed comparedKey) []string {
	var why []string
	if k.revoked.Valid && !backed.revoked.Valid {
		why = append(why, revokedHere)
	}
	if !k.revoked.Valid && backed.revoked.Valid {
		why = append(why, "revoked in this backup and not on this root")
	}
	if k.hash != backed.hash || k.prefix != backed.prefix || k.rotated != backed.rotated {
		why = append(why, "rotated")
	}
	if k.scopes.Valid && k.scopes != backed.scopes {
		why = append(why, "permissions changed")
	}
	if k.expires != backed.expires {
		why = append(why, "expiry changed")
	}
	if k.name != backed.name {
		why = append(why, "renamed")
	}
	return why
}

// How a key revoked on this root, and not in the backup, differs. On the list of keys that stay as they
// are, "revoked" is what stays true. On the list of keys going back it's what changes, and "revoked" alone
// would read as if it stays that way.
const (
	revokedHere    = "revoked"
	revokedAndBack = "revoked on this root, and would work again"
)

// broughtBackWords says how a key named to come back differs. Revoked is always first, when it's there.
func broughtBackWords(why []string) string {
	words := slices.Clone(why)
	if len(words) > 0 && words[0] == revokedHere {
		words[0] = revokedAndBack
	}
	return strings.Join(words, ", ")
}

// scopeList is a key's permissions. A list that doesn't read is shown as it stands, which is also more
// than making requests.
func (k comparedKey) scopeList() []string {
	var scopes []string
	if err := json.Unmarshal([]byte(k.scopes.String), &scopes); err != nil {
		return []string{k.scopes.String}
	}
	return scopes
}

// beyondRequests lists what a key may do beyond what a new key gets, which the code calls no
// administration. Anything else counts, so a permission added later, or one only a file has heard of,
// is treated as the stronger kind.
func (k comparedKey) beyondRequests() []string {
	scopes := slices.DeleteFunc(k.scopeList(), func(s string) bool { return slices.Contains(auth.DefaultKeyScopes, s) })
	slices.Sort(scopes)
	return scopes
}

// permissions is what the report says a list of permissions comes to.
func permissions(scopes []string) string {
	if len(scopes) == 0 {
		return "no permissions"
	}
	return strings.Join(scopes, ", ")
}

// holdBack keeps a key that can do more than make requests out of a restore, unless it's named.
func holdBack(ctx context.Context, staged *sql.DB, id string, k comparedKey, beyond []string, named, used map[string]bool, rep *Report) error {
	if named[id] {
		used[id] = true
		rep.KeysBroughtBack = append(rep.KeysBroughtBack, KeyChange{id, k.name, "can do " + permissions(beyond)})
		return nil
	}
	if _, err := staged.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id); err != nil {
		return err
	}
	rep.KeysHeldBack = append(rep.KeysHeldBack, KeyChange{id, k.name, permissions(beyond)})
	return nil
}

// holdBackKeys is for a restore with no keys of a root's to set a backup's against: a new root, a root
// with no keys, or one that can't be read. An ordinary key comes, since getting its clients back is
// what a backup is for. A key that can do more doesn't, since this is just where a file would bring in
// an administrator's key unnoticed.
func holdBackKeys(ctx context.Context, staged *sql.DB, rep *Report, bringBack []string) error {
	backed, err := readComparedKeys(ctx, staged)
	if err != nil {
		return err
	}
	named, used := keyNames(bringBack), map[string]bool{}
	for id, k := range backed {
		if k.revoked.Valid {
			continue // A revoked key does nothing, here or not.
		}
		if beyond := k.beyondRequests(); len(beyond) > 0 {
			if err := holdBack(ctx, staged, id, k, beyond, named, used, rep); err != nil {
				return err
			}
		}
	}
	return checkKeyNames(bringBack, used)
}

func keyNames(bringBack []string) map[string]bool {
	named := map[string]bool{}
	for _, id := range bringBack {
		named[id] = true
	}
	return named
}

// checkKeyNames refuses a name that did nothing. It may be a mistyped id, with the real key left on its
// current secret.
func checkKeyNames(bringBack []string, used map[string]bool) error {
	for _, id := range bringBack {
		if !used[id] {
			return refuse("--bring-back-key %s: there's nothing to bring back. Either this backup doesn't hold that key, or it would change nothing here.", id)
		}
	}
	return nil
}

// activeKeys lists every key that would work, with what it may do, by the gateway's own reading.
func activeKeys(ctx context.Context, st *store.Store) ([]KeyChange, error) {
	keys, err := st.ListAPIKeys(ctx)
	if err != nil {
		return nil, refuse("The API keys in this backup can't be read: %v", err)
	}
	now := time.Now()
	var out []KeyChange
	for i := range keys {
		if keys[i].Active(now) {
			out = append(out, KeyChange{keys[i].ID, keys[i].Name, permissions(keys[i].Scopes)})
		}
	}
	sortKeys(out)
	return out, nil
}

func sortKeys(keys []KeyChange) {
	slices.SortFunc(keys, func(a, b KeyChange) int { return strings.Compare(a.ID, b.ID) })
}

type comparedUser struct{ status, email string }

func readComparedUsers(ctx context.Context, db *sql.DB) (map[string]comparedUser, error) {
	out := map[string]comparedUser{}
	present, err := tableExists(ctx, db, "auth_users")
	if err != nil || !present {
		return out, err
	}
	emails, err := tableExists(ctx, db, "auth_user_emails")
	if err != nil {
		return nil, err
	}
	query := `SELECT id, status, '' FROM auth_users`
	if emails {
		query = `SELECT u.id, u.status, COALESCE(e.email, '') FROM auth_users u LEFT JOIN auth_user_emails e ON e.user_id = u.id`
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var u comparedUser
		if err := rows.Scan(&id, &u.status, &u.email); err != nil {
			return nil, err
		}
		out[id] = u
	}
	return out, rows.Err()
}

// compareWithTarget keeps the keys of the root being replaced as they are, and says so. Restoring must
// not revive a key someone revoked or rotated because it leaked, and a file mustn't add one to a root
// that has its own. Keys named in bringBack take the backup's version, and the report names them. It
// says whether every key could be compared.
func compareWithTarget(ctx context.Context, staged *sql.DB, targetDB string, liveErr error, rep *Report, bringBack []string, o options) (compared bool, err error) {
	// hasTable reads an error as "no such table", which here would say that nothing is revoked. A
	// root is often replaced because its database is broken, so this can't refuse, but it says so.
	err = liveErr
	if err == nil {
		err = retryBusy(ctx, func() error { return readableDatabase(ctx, targetDB) })
	}
	if isBusy(err) {
		return false, fmt.Errorf("reading this root's database: %w", err)
	}
	if err != nil {
		if err := holdBackKeys(ctx, staged, rep, bringBack); err != nil {
			return false, err
		}
		rep.Cautions = append(rep.Cautions, fmt.Sprintf("The database on this root can't be read (%v), so nothing could be compared with it. Any key revoked or rotated since this backup was made works again here, and anyone removed comes back. Check them afterwards: local-base keys list", err))
		return false, nil
	}
	o.targetReadDone()
	cur, err := openReadOnly(targetDB)
	if err != nil {
		return false, err
	}
	defer cur.Close()
	now, err := readComparedKeys(ctx, cur)
	if err != nil {
		return false, err
	}
	if compared, err = compareKeys(ctx, staged, now, rep, bringBack); err != nil {
		return false, err
	}
	if rep.ReturningBindings, err = returningBindings(ctx, staged, cur); err != nil {
		return false, err
	}

	usersNow, err := readComparedUsers(ctx, cur)
	if err != nil {
		return false, err
	}
	usersBacked, err := readComparedUsers(ctx, staged)
	if err != nil {
		return false, err
	}
	// An id survives within one root's lineage. The address is the fallback.
	byEmail := map[string]comparedUser{}
	for _, u := range usersNow {
		if u.email != "" {
			byEmail[u.email] = u
		}
	}
	for id, u := range usersBacked {
		was, here := usersNow[id]
		if !here && u.email != "" {
			was, here = byEmail[u.email]
		}
		switch {
		case !here:
			if u.status != "removed" {
				rep.ReturningUsers++
			}
		case was.status == u.status:
		case was.status == "removed" || was.status == "disabled":
			if _, err := staged.ExecContext(ctx, `UPDATE auth_users SET status = ? WHERE id = ?`, was.status, id); err != nil {
				return false, err
			}
			rep.UserStatusesKept++
		default:
			rep.UsersChanged++
		}
	}
	return compared, nil
}

// compareKeys sets the backup's keys against the root's, and says whether it could set all of them. A
// root with none has nothing to compare, as a new root doesn't. A root older than key permissions can't
// say what its keys may do.
func compareKeys(ctx context.Context, staged *sql.DB, now map[string]comparedKey, rep *Report, bringBack []string) (bool, error) {
	if len(now) == 0 {
		return false, holdBackKeys(ctx, staged, rep, bringBack)
	}
	backed, err := readComparedKeys(ctx, staged)
	if err != nil {
		return false, err
	}
	compared := true
	for _, k := range now {
		compared = compared && k.scopes.Valid
	}
	named, used := keyNames(bringBack), map[string]bool{}
	for id, k := range backed {
		was, here := now[id]
		switch {
		case !here:
			switch {
			case k.revoked.Valid: // A revoked key does nothing, here or not.
			case named[id]:
				used[id] = true
				rep.KeysBroughtBack = append(rep.KeysBroughtBack, KeyChange{id, k.name, "this root doesn't have it"})
			default:
				if _, err := staged.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id); err != nil {
					return false, err
				}
				rep.KeysLeftOut = append(rep.KeysLeftOut, KeyChange{id, k.name, permissions(k.scopeList())})
			}
		default:
			// A root that can't say what its key may do can't vouch for what a file says it may.
			if !was.scopes.Valid && !k.revoked.Valid {
				if beyond := k.beyondRequests(); len(beyond) > 0 {
					if err := holdBack(ctx, staged, id, k, beyond, named, used, rep); err != nil {
						return false, err
					}
					continue
				}
			}
			why := was.differences(k)
			if len(why) == 0 {
				continue
			}
			text := strings.Join(why, ", ")
			if named[id] {
				used[id] = true
				rep.KeysBroughtBack = append(rep.KeysBroughtBack, KeyChange{id, k.name, broughtBackWords(why)})
				continue
			}
			// What the root has wins, whole, so a key can't come back partly.
			if _, err := staged.ExecContext(ctx, `UPDATE api_keys SET name = ?, prefix = ?, key_hash = ?, scopes = COALESCE(?, scopes), last_rotated_at = ?, expires_at = ?, revoked_at = ? WHERE id = ?`,
				was.name, was.prefix, was.hash, was.scopes, was.rotated, was.expires, was.revoked, id); err != nil {
				return false, err
			}
			rep.KeysKept = append(rep.KeysKept, KeyChange{id, was.name, text})
			if was.revoked.Valid && !k.revoked.Valid {
				rep.RevocationsKept++
			}
		}
	}
	if err := checkKeyNames(bringBack, used); err != nil {
		return false, err
	}
	for id, k := range now {
		if _, ok := backed[id]; !ok && !k.revoked.Valid {
			rep.KeysLost = append(rep.KeysLost, KeyChange{ID: id, Name: k.name})
		}
	}
	return compared, nil
}
