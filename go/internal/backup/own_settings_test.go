package backup

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
)

// What a crafted file would carry in place of what the last check for a newer version found: a
// version of its own, a link of its own, the tag it wants kept, and a check time that never comes due.
var plantedUpdateCache = []string{
	`INSERT OR REPLACE INTO go_settings (key, value, updated_at) VALUES ('update.latest', 'v99.0.0', 'x')`,
	`INSERT OR REPLACE INTO go_settings (key, value, updated_at) VALUES ('update.url', 'https://planted.example.com/release', 'x')`,
	`INSERT OR REPLACE INTO go_settings (key, value, updated_at) VALUES ('update.etag', '"planted-etag"', 'x')`,
	`INSERT OR REPLACE INTO go_settings (key, value, updated_at) VALUES ('update.checked-at', '2999-01-01T00:00:00Z', 'x')`,
}

func setting(key, value string) string {
	return `INSERT OR REPLACE INTO go_settings (key, value, updated_at) VALUES ('` + key + `', '` + value + `', 'x')`
}

// rootWithSettings is a real root holding these rows, and a backup of it.
func rootWithSettings(t *testing.T, statements ...string) (rootDir, file string) {
	t.Helper()
	r, _ := newRoot(t, dir(t))
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range statements {
		if _, err := st.DB.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return r.Dir, create(t, r, false).Output
}

// settingsIn reads every stored setting of a database, by opening a copy of it.
func settingsIn(t *testing.T, path string) map[string]string {
	t.Helper()
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT key, value FROM go_settings`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k string
		var v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v.String
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// settingsInBackup reads the stored settings of a backup's database.
func settingsInBackup(t *testing.T, file string) map[string]string {
	t.Helper()
	for _, e := range read(t, file) {
		if e.header.Name == filesPrefix+"local-base.db" {
			p := filepath.Join(dir(t), "local-base.db")
			if err := os.WriteFile(p, e.body, 0o600); err != nil {
				t.Fatal(err)
			}
			return settingsIn(t, p)
		}
	}
	t.Fatal("no database in the backup")
	return nil
}

// What the last check for a newer version found isn't a setting. A backup doesn't carry it, and a file
// that does gets none of it onto a root, so no version or link of its own shows as available.
func TestABackupLeavesOutWhatTheUpdateCheckFound(t *testing.T) {
	_, file := rootWithSettings(t, append([]string{setting("update.check", "on")}, plantedUpdateCache...)...)
	held := settingsInBackup(t, file)
	for _, key := range []string{"update.latest", "update.url", "update.etag", "update.checked-at"} {
		if _, ok := held[key]; ok {
			t.Errorf("the backup holds %s", key)
		}
	}
	if held["update.check"] != "on" {
		t.Errorf("the setting itself should stay: %q", held["update.check"])
	}

	bad := editedBackup(t, file, plantedUpdateCache...)
	target := filepath.Join(dir(t), "target")
	restore(t, bad, target, false)
	restored := settingsIn(t, filepath.Join(target, "local-base.db"))
	for _, key := range []string{"update.latest", "update.url", "update.etag", "update.checked-at"} {
		if v, ok := restored[key]; ok {
			t.Errorf("the restored root holds %s = %q", key, v)
		}
	}
	st, err := store.Open(filepath.Join(target, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	state := upgrade.NewChecker(st, upgrade.Source{}, "1.0.0", true, slog.New(slog.NewTextHandler(io.Discard, nil))).Read(context.Background())
	if state.UpdateAvailable || state.Latest != nil || state.URL != nil {
		t.Errorf("the settings page would show the file's own answer: %+v", state)
	}
}

// askCounter answers every request with a failure, and counts them. Nothing reaches a network.
type askCounter struct{ asked int }

func (a *askCounter) RoundTrip(*http.Request) (*http.Response, error) {
	a.asked++
	return nil, errors.New("no network in this test")
}

// A check time in the future would stop the daily check for good. After a restore, the check asks.
func TestAFutureCheckTimeInAFileDoesNotStopTheCheck(t *testing.T) {
	_, file := rootWithSettings(t, setting("update.check", "on"))
	bad := editedBackup(t, file, plantedUpdateCache...)
	target := filepath.Join(dir(t), "target")
	restore(t, bad, target, false)
	st, err := store.Open(filepath.Join(target, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	counter := &askCounter{}
	src := upgrade.Source{API: "https://api.github.com/repos/x/y/releases/latest", Client: &http.Client{Transport: counter}}
	c := upgrade.NewChecker(st, src, "1.0.0", true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !c.CheckIfDue(context.Background()) || counter.asked != 1 {
		t.Errorf("the check didn't ask: due=%v asked=%d", c.CheckIfDue(context.Background()), counter.asked)
	}
}

func ownSetting(rep *Report, key string) OwnSetting {
	for _, s := range rep.OwnSettings {
		if s.Setting == key {
			return s
		}
	}
	return OwnSetting{Setting: "missing: " + key}
}

// The report says what the root will do by itself, by what each setting does, set against the root
// being replaced: the same, different, a new root, and one that can't be read.
func TestTheReportSaysWhatTheRootDoesByItself(t *testing.T) {
	fileSays := []string{setting("update.check", "on"), setting(runtime.SpeechMarkingKey, "off")}
	t.Run("the same", func(t *testing.T) {
		rootDir, file := rootWithSettings(t, fileSays...)
		rep := stageReport(t, file, rootDir, true)
		for key, want := range map[string]OwnSetting{
			"update.check":           {Setting: "update.check", Value: "on", ThisRoot: "on"},
			runtime.SpeechMarkingKey: {Setting: runtime.SpeechMarkingKey, Value: "off", ThisRoot: "off"},
		} {
			if got := ownSetting(rep, key); got != want {
				t.Errorf("%+v, want %+v", got, want)
			}
		}
		if caution(rep, markingOffCaution) || caution(rep, markingUnknownCaution) {
			t.Errorf("nothing changes, so nothing to caution: %v", rep.Cautions)
		}
	})
	t.Run("different", func(t *testing.T) {
		rootDir, file := rootWithSettings(t)
		bad := editedBackup(t, file, fileSays...)
		rep := stageReport(t, bad, rootDir, true)
		for key, want := range map[string]OwnSetting{
			"update.check":           {Setting: "update.check", Value: "on", ThisRoot: "off"},
			runtime.SpeechMarkingKey: {Setting: runtime.SpeechMarkingKey, Value: "off", ThisRoot: "on"},
		} {
			if got := ownSetting(rep, key); got != want {
				t.Errorf("%+v, want %+v", got, want)
			}
		}
		if !caution(rep, markingOffCaution) {
			t.Errorf("no caution: %v", rep.Cautions)
		}
	})
	t.Run("a new root", func(t *testing.T) {
		_, file := rootWithSettings(t, fileSays...)
		rep := stageReport(t, file, filepath.Join(dir(t), "target"), false)
		for key, want := range map[string]OwnSetting{
			"update.check":           {Setting: "update.check", Value: "on"},
			runtime.SpeechMarkingKey: {Setting: runtime.SpeechMarkingKey, Value: "off"},
		} {
			if got := ownSetting(rep, key); got != want {
				t.Errorf("%+v, want %+v", got, want)
			}
		}
		if !caution(rep, markingOffCaution) {
			t.Errorf("no caution: %v", rep.Cautions)
		}
	})
	t.Run("a root that can't be read", func(t *testing.T) {
		rootDir, file := rootWithSettings(t, fileSays...)
		if err := os.WriteFile(filepath.Join(rootDir, "local-base.db"), []byte(strings.Repeat("not a database ", 400)), 0o600); err != nil {
			t.Fatal(err)
		}
		rep := stageReport(t, file, rootDir, true)
		for _, key := range []string{"update.check", runtime.SpeechMarkingKey} {
			if got := ownSetting(rep, key); !got.ThisRootUnreadable || got.ThisRoot != "" {
				t.Errorf("%+v", got)
			}
		}
		if !caution(rep, markingUnknownCaution) || caution(rep, markingOffCaution) {
			t.Errorf("it should say it may change, not that it does: %v", rep.Cautions)
		}
	})
	t.Run("a backup without them", func(t *testing.T) {
		rootDir, file := rootWithSettings(t, fileSays...)
		bad := editedBackup(t, file, `DELETE FROM go_settings WHERE key IN ('update.check', '`+runtime.SpeechMarkingKey+`')`)
		rep := stageReport(t, bad, rootDir, true)
		for key, want := range map[string]OwnSetting{
			"update.check":           {Setting: "update.check", Value: "off", ThisRoot: "on"},
			runtime.SpeechMarkingKey: {Setting: runtime.SpeechMarkingKey, Value: "on", ThisRoot: "off"},
		} {
			if got := ownSetting(rep, key); got != want {
				t.Errorf("%+v, want %+v", got, want)
			}
		}
	})
}

// The words a person reads, for each case.
func TestTheOwnSettingLinesRead(t *testing.T) {
	for _, c := range []struct {
		in      OwnSetting
		newRoot bool
		want    string
	}{
		{OwnSetting{Setting: "update.check", Value: "on", ThisRoot: "off"}, false, "Checks GitHub once a day for a newer version: on (this root has it off)"},
		{OwnSetting{Setting: "update.check", Value: "off", ThisRoot: "off"}, false, "Checks GitHub once a day for a newer version: off"},
		{OwnSetting{Setting: "update.check", Value: "on"}, true, "Checks GitHub once a day for a newer version: on (a new root has it off)"},
		{OwnSetting{Setting: "update.check", Value: "off"}, true, "Checks GitHub once a day for a newer version: off"},
		{OwnSetting{Setting: "update.check", Value: "on", ThisRootUnreadable: true}, false, "Checks GitHub once a day for a newer version: on (this root's setting can't be read)"},
		{OwnSetting{Setting: runtime.SpeechMarkingKey, Value: "off", ThisRoot: "on"}, false, "Marks synthesized speech as made by AI: off (this root has it on)"},
		{OwnSetting{Setting: runtime.SpeechMarkingKey, Value: "on", ThisRoot: "off"}, false, "Marks synthesized speech as made by AI: on (this root has it off)"},
		{OwnSetting{Setting: runtime.SpeechMarkingKey, Value: "on", ThisRoot: "on"}, false, "Marks synthesized speech as made by AI: on"},
		{OwnSetting{Setting: runtime.SpeechMarkingKey, Value: "off"}, true, "Marks synthesized speech as made by AI: off (a new root has it on)"},
		{OwnSetting{Setting: runtime.SpeechMarkingKey, Value: "on"}, true, "Marks synthesized speech as made by AI: on"},
		{OwnSetting{Setting: runtime.SpeechMarkingKey, Value: "off", ThisRootUnreadable: true}, false, "Marks synthesized speech as made by AI: off (this root's setting can't be read)"},
	} {
		if got := c.in.Text(c.newRoot); got != c.want {
			t.Errorf("%+v, new root %v:\n got %q\nwant %q", c.in, c.newRoot, got, c.want)
		}
	}
	if markingOffCaution != `This turns off marking synthesized speech as made by AI. Telling people it is becomes your responsibility. See "What this root does by itself" above.` {
		t.Errorf("%q", markingOffCaution)
	}
	if markingUnknownCaution != `This may turn off marking synthesized speech as made by AI, since this root's setting can't be read. See "What this root does by itself" above.` {
		t.Errorf("%q", markingUnknownCaution)
	}
}

// Marking turned off is cautioned only when it changes something here, or might.
func TestTurningSpeechMarkingOffIsCautioned(t *testing.T) {
	values := func(marking string) map[string]string {
		return map[string]string{"update.check": "off", runtime.SpeechMarkingKey: marking}
	}
	unreadable := errors.New("unreadable")
	for _, c := range []struct {
		name    string
		file    string
		live    map[string]string
		liveErr error
		newRoot bool
		want    string
	}{
		{"off over on", "off", values("on"), nil, false, markingOffCaution},
		{"off onto a new root", "off", nil, nil, true, markingOffCaution},
		{"off over a root that can't be read", "off", nil, unreadable, false, markingUnknownCaution},
		{"off over off", "off", values("off"), nil, false, ""},
		{"on over off", "on", values("off"), nil, false, ""},
		{"on over on", "on", values("on"), nil, false, ""},
		{"on onto a new root", "on", nil, nil, true, ""},
		{"on over a root that can't be read", "on", nil, unreadable, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, got := ownSettingsReport(values(c.file), c.live, c.liveErr, c.newRoot); got != c.want {
				t.Errorf("%q, want %q", got, c.want)
			}
		})
	}
}

// The report takes each value the way the code that acts on it does, so a value the file spells oddly
// can't read as one thing and do another.
func TestTheReportReadsSettingsAsTheirReadersDo(t *testing.T) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, raw := range []*string{nil, ptr("on"), ptr("off"), ptr("OFF"), ptr("On"), ptr(""), ptr("yes"), ptr(" off"), ptr("off "), ptr("0")} {
		name := "absent"
		if raw != nil {
			name = "\"" + *raw + "\""
		}
		t.Run(name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(dir(t), "local-base.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if raw != nil {
				for _, key := range []string{"update.check", runtime.SpeechMarkingKey} {
					if err := st.SetSetting(ctx, key, *raw); err != nil {
						t.Fatal(err)
					}
				}
			}
			got, err := readOwnSettings(ctx, st.DB)
			if err != nil {
				t.Fatal(err)
			}
			onOff := map[bool]string{true: "on", false: "off"}
			if want := onOff[upgrade.NewChecker(st, upgrade.Source{}, "1.0.0", true, quiet).Enabled(ctx)]; got["update.check"] != want {
				t.Errorf("update.check: the report says %q, and the check is %q", got["update.check"], want)
			}
			if want := onOff[runtime.SpeechMarking(ctx, st)]; got[runtime.SpeechMarkingKey] != want {
				t.Errorf("speech marking: the report says %q, and marking is %q", got[runtime.SpeechMarkingKey], want)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// A database older than the settings table has the defaults, and says so rather than failing.
func TestADatabaseWithoutSettingsHasTheDefaults(t *testing.T) {
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(dir(t), "old.db"), ""))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE something_else (x)`); err != nil {
		t.Fatal(err)
	}
	got, err := readOwnSettings(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if got["update.check"] != "off" || got[runtime.SpeechMarkingKey] != "on" {
		t.Errorf("%v", got)
	}
}

// A settings table that is there and can't be read is an error, not the defaults.
func TestAFailedReadOfTheSettingsIsAnError(t *testing.T) {
	db, err := sql.Open("sqlite", sqliteDSN(filepath.Join(dir(t), "broken.db"), ""))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE go_settings (key TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := readOwnSettings(context.Background(), db); err == nil {
		t.Error("a settings table that can't be read gave an answer")
	}
}
