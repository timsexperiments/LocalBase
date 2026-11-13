package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

// validate checks everything else, with the validators the gateway uses, and writes the staged
// database's fixes. It is the staged copy, and nothing else is touched.
func validate(ctx context.Context, dir, target string, m *Manifest, state *targetState, bringBack []string) (*Report, error) {
	dbPath := filepath.Join(dir, "local-base.db")
	if err := checkDatabase(ctx, dbPath, state.opts); err != nil {
		return nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, refuse("The database can't be used: %v", err)
	}
	defer func() { _ = st.Close() }() // Every write below has committed or returned its own error.
	st.DB.SetMaxOpenConns(1)
	// What a backup leaves out is removed, not trusted to be absent: a crafted file can carry
	// sessions, and a restore has to sign everyone out regardless.
	if err := dropRows(ctx, st.DB); err != nil {
		return nil, err
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE config SET root = ?`, target); err != nil {
		return nil, err
	}
	cfg, err := st.LoadConfig(ctx)
	if err != nil && !errors.Is(err, store.ErrNotConfigured) {
		return nil, refuse("The settings can't be read: %v", err)
	}
	// The gateway's own checks of roles, permissions, addresses and duplicates.
	policy, err := auth.LoadPolicy(ctx, st.DB)
	if err != nil {
		return nil, refuse("The access policy in this backup can't be read: %v", err)
	}
	if policy != nil {
		if err := policy.Normalize(); err != nil {
			return nil, refuse("The access policy in this backup is not valid: %v", err)
		}
	}

	rep := &Report{Target: target, NewRoot: state.newRoot, Manifest: manifestToShow(m)}
	access, err := auth.LoadAccess(dir)
	if err != nil {
		return nil, refuse("ui-access.json in this backup is not valid: %v", err)
	}
	if access != nil {
		for _, r := range access.Provider.Registrations {
			rep.Providers = append(rep.Providers, r.Kind+":"+r.ID)
		}
		if access.Provider.Kind != "direct" {
			rep.Providers = append(rep.Providers, access.Provider.Kind)
		}
	}
	if _, err := auth.LoadEmail(dir); err != nil {
		return nil, refuse("email-delivery.json in this backup is not valid: %v", err)
	}
	// Checked as every use of them checks them. An origin the huggingface profile can't name is left out wherever
	// the profile is used, so it isn't a reason to refuse.
	profiles, err := acquire.LoadProfiles(dir)
	if err == nil {
		err = profiles.FirstInvalid()
	}
	if err != nil {
		return nil, refuse("The download credentials in this backup are not valid: %v", err)
	}
	if rep.Downloads, err = downloadCredentials(profiles); err != nil {
		return nil, err
	}
	if rep.Programs, err = checkBackends(dir); err != nil {
		return nil, err
	}
	cat, err := checkCatalog(dir, cfg)
	if err != nil {
		return nil, err
	}
	if rep.Catalog, err = catalogEntries(cat); err != nil {
		return nil, err
	}
	if err := checkVoices(ctx, dir, st); err != nil {
		return nil, err
	}
	if rep.Sources, err = checkImported(ctx, st); err != nil {
		return nil, err
	}
	if err := fillCounts(ctx, st, rep); err != nil {
		return nil, err
	}
	if err := checkBindingRows(ctx, st.DB); err != nil {
		return nil, err
	}
	backedSignIn := readSignIn(ctx, dir, st.DB)
	if backedSignIn.policyErr != nil {
		return nil, refuse("The access policy in this backup can't be read: %v", backedSignIn.policyErr)
	}
	var liveValues *signInValues
	if !state.newRoot && state.hasData {
		v := liveSignIn(ctx, target, state.liveDB, state.liveErr, state.opts)
		liveValues = &v
	}
	var signInCaution string
	rep.SignIn, signInCaution = signInReport(backedSignIn, liveValues)
	if signInCaution != "" {
		rep.Cautions = append(rep.Cautions, signInCaution)
	}
	if rep.Grants, err = readGrants(ctx, st.DB); err != nil {
		return nil, refuse("The roles in this backup can't be read: %v", err)
	}
	if !state.newRoot && state.hasData {
		live, liveErr := liveGrants(ctx, state.liveDB, state.liveErr, state.opts)
		gone, changed := compareGrants(rep.Grants, live, liveErr)
		rep.RolesGone = gone
		switch {
		case changed && liveErr != nil:
			rep.Cautions = append(rep.Cautions, grantsUnknownCaution)
		case changed:
			rep.Cautions = append(rep.Cautions, grantsChangedCaution)
		}
	}
	if rep.Issuers, rep.Unbound, err = bindingsOf(ctx, st.DB, access); err != nil {
		return nil, refuse("The access bindings in this backup can't be read: %v", err)
	}
	rep.UnboundNote = unboundNote(rep.Unbound, access)
	if cfg != nil {
		rep.Ports = []int{cfg.GatewayPort, cfg.Port, cfg.SttPort}
		// A backend has no authentication, so it only ever listens on loopback, whatever its stored
		// host says. The report says where it will listen, not what is stored.
		rep.Settings = append(rep.Settings, Setting{"gateway, reachable from " + reach(cfg.GatewayHost), fmt.Sprintf("%s:%d", cfg.GatewayHost, cfg.GatewayPort)},
			Setting{"chat backend, reachable from this computer only", fmt.Sprintf("%s:%d", runtime.BackendHost(cfg.Host), cfg.Port)},
			Setting{"speech backend, reachable from this computer only", fmt.Sprintf("%s:%d", runtime.BackendHost(cfg.SttHost), cfg.SttPort)})
		if cfg.OtelEndpoint != "" {
			rep.Settings = append(rep.Settings, Setting{"OTLP endpoint: traces and logs go here", cfg.OtelEndpoint})
		}
		if cfg.OtelHeaders != "" {
			rep.Settings = append(rep.Settings, Setting{"OTLP headers", "set"})
		}
	}
	exposure, err := exposureSettings(ctx, st.DB)
	if err != nil {
		return nil, err
	}
	rep.Settings = append(rep.Settings, exposure...)
	backedOwn, err := backupOwnSettings(ctx, st.DB)
	if err != nil {
		return nil, err
	}
	// A root with no data has what a new one has, and it is set against that by name: "this root".
	liveOwn, liveOwnErr := map[string]string{}, error(nil)
	for _, o := range ownSettings {
		liveOwn[o.key] = o.byDefault
	}
	if !state.newRoot && state.hasData {
		liveOwn, liveOwnErr = liveOwnSettings(ctx, state.liveDB, state.liveErr, state.opts)
	}
	var ownCaution string
	rep.OwnSettings, ownCaution = ownSettingsReport(backedOwn, liveOwn, liveOwnErr, state.newRoot)
	if ownCaution != "" {
		rep.Cautions = append(rep.Cautions, ownCaution)
	}
	compared := false
	if !state.newRoot && state.hasData {
		if compared, err = compareWithTarget(ctx, st.DB, state.liveDB, state.liveErr, rep, bringBack, state.opts); err != nil {
			return nil, err
		}
	} else {
		if err := holdBackKeys(ctx, st.DB, rep, bringBack); err != nil {
			return nil, err
		}
		rep.Cautions = append(rep.Cautions, "Any key revoked or rotated since this backup was made works again here. Check them afterwards: local-base keys list")
	}
	for _, list := range [][]KeyChange{rep.KeysKept, rep.KeysLeftOut, rep.KeysBroughtBack, rep.KeysLost, rep.KeysHeldBack} {
		sortKeys(list)
	}
	if !compared {
		if rep.KeysActive, err = activeKeys(ctx, st); err != nil {
			return nil, err
		}
	}
	// What it holds, read the way a backup reads a root, once each file has passed its own check. A file
	// can say anything about itself.
	held, err := readSecrets(ctx, dir, dbPath, false, state.opts)
	if err != nil {
		return nil, refuse("The secrets in this backup can't be read: %v", err)
	}
	rep.Secrets = held.names
	for _, name := range held.names {
		if !slices.Contains(m.SecretNames, name) {
			rep.SecretsUnlisted = append(rep.SecretsUnlisted, name)
		}
	}
	for _, r := range m.SecretsRemoved {
		rep.PutBack = append(rep.PutBack, putBack(r))
	}
	// This is the staged copy. Nothing has touched the root yet, whichever command is asking: a restore that's
	// still staging, or a backup checking that it would restore.
	if err := state.opts.settleJournal(ctx, st.DB); err != nil {
		var stayed *modeStayedError
		if errors.As(err, &stayed) {
			return nil, refuse("This backup's database can't be used: its journal mode stayed %s, so nothing was changed.", stayed.mode)
		}
		return nil, refuse("The database can't be used: %v", err)
	}
	return rep, nil
}

func checkBackends(dir string) ([]Program, error) {
	files, err := os.ReadDir(backends.Dir(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	builtin, err := backends.Load("")
	if err != nil {
		return nil, err
	}
	var out []Program
	for _, e := range files {
		d, err := backendProblem(filepath.Join(backends.Dir(dir), e.Name()), builtin)
		if err != nil {
			return nil, refuse("The backend %s in this backup is not valid: %v", e.Name(), err)
		}
		_, statErr := os.Stat(d.Launch.Command)
		out = append(out, Program{Backend: d.ID, Command: append([]string{d.Launch.Command}, d.Launch.Args...), Exists: statErr == nil})
	}
	return out, nil
}

// checkCatalog is what opening the root will do, so a root that would refuse every command is
// caught here. It returns the catalog it loaded, for the report.
func checkCatalog(dir string, cfg *store.Config) (*catalog.Catalog, error) {
	if b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(formerLog))); err == nil {
		for _, id := range strings.Split(string(b), "\n") {
			if id = strings.TrimSpace(id); id != "" && !formerIDRe.MatchString(id) {
				return nil, refuse("%s in this backup is not a list of ids.", formerLog)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	cat, err := catalog.LoadRoot(dir, nil)
	if err != nil {
		return nil, refuse("The catalog in this backup is not valid: %v", err)
	}
	// The gateway only reports a bad entry. A backup leaves them out, so one here was put there.
	for _, d := range cat.Diagnostics {
		if d.Tier == "user" {
			return nil, refuse("The catalog entry %s in this backup is not valid: %s", filepath.Base(d.Path), d.Message)
		}
	}
	if cfg == nil {
		return cat, nil
	}
	conf.KeepFormerSelections(dir, cfg, cat)
	if issues := conf.ValidateModels(cfg, cat, false); len(issues) > 0 {
		return nil, refuse("The settings in this backup are not valid: %s: %s", issues[0].Path, issues[0].Message)
	}
	return cat, nil
}

func checkVoices(ctx context.Context, dir string, st *store.Store) error {
	rows, err := st.Voices(ctx)
	if err != nil {
		return refuse("The voices in this backup can't be read: %v", err)
	}
	for _, v := range rows {
		p, ok := voices.Path(dir, v.ID)
		if !ok {
			return refuse("A voice in this backup has an unusable id: %q.", v.ID)
		}
		f, err := describe(p, v.ID)
		if err != nil {
			return refuse("The voice %s is in this backup's database, and its recording is missing.", v.ID)
		}
		if f.SHA256 != strings.ToLower(v.SHA256) {
			return refuse("The recording of voice %s does not match its database row.", v.ID)
		}
	}
	return nil
}

// checkImported validates rows that the file validators don't reach: a definition is JSON that
// names files and where they come from.
func checkImported(ctx context.Context, st *store.Store) ([]ModelSource, error) {
	rows, err := st.ImportedModels(ctx)
	if err != nil {
		return nil, refuse("The imported models in this backup can't be read: %v", err)
	}
	var out []ModelSource
	for _, row := range rows {
		var m models.Model
		if err := json.Unmarshal(row.Definition, &m); err != nil {
			return nil, refuse("The imported model %q in this backup can't be read: %v", row.ID, err)
		}
		if m.ID != row.ID {
			return nil, refuse("The imported model %q in this backup has a definition for %q.", row.ID, m.ID)
		}
		if err := m.Validate(); err != nil {
			return nil, refuse("The imported model %q in this backup is not valid: %v", row.ID, err)
		}
		for _, v := range m.Variants {
			for _, a := range v.Artifacts {
				if !filepath.IsLocal(a.Filename) || strings.ContainsAny(a.Filename, `/\`) {
					return nil, refuse("The imported model %q in this backup names an unsafe file: %q.", row.ID, a.Filename)
				}
				switch a.Source.Kind {
				case "local":
					out = append(out, ModelSource{m.ID, "reads this path", a.Source.Path})
				case "url":
					out = append(out, ModelSource{m.ID, "downloads from", a.Source.URL})
				}
			}
		}
	}
	return out, nil
}

func fillCounts(ctx context.Context, st *store.Store, rep *Report) error {
	keys, err := st.ListAPIKeys(ctx)
	if err != nil {
		return refuse("The API keys in this backup can't be read: %v", err)
	}
	now := time.Now()
	for _, k := range keys {
		rep.Keys.Total++
		switch {
		case k.RevokedAt != nil:
			rep.Keys.Revoked++
		case k.Active(now):
			rep.Keys.Active++
		default:
			rep.Keys.Expired++
		}
	}
	present, err := tableExists(ctx, st.DB, "auth_users")
	if err != nil || !present {
		return err
	}
	return st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_users`).Scan(&rep.Users)
}

// reach says plainly who could reach an address, for the report.
func reach(host string) string {
	bare := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(host), "["), "]")
	if runtime.BackendHost(host) == bare {
		return "this computer only"
	}
	return "other computers too"
}

// exposureSettings are stored settings that change who can reach this root, or where it reaches.
func exposureSettings(ctx context.Context, db *sql.DB) ([]Setting, error) {
	present, err := tableExists(ctx, db, "go_settings")
	if err != nil || !present {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM go_settings WHERE key LIKE 'imports.%' OR key LIKE 'gateway.%' ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Setting
	for rows.Next() {
		var s Setting
		if err := rows.Scan(&s.Name, &s.Value); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
