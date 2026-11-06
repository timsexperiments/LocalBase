package backup

import (
	"context"
	"database/sql"
	"errors"

	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// What a root does by itself, with nobody asking: settings whose effect none of the report's other lines
// shows. Each is shown by what it does, read the way the code that acts on it reads it, and set against
// the root being replaced.

// updateCache is what the last check for a newer version found. It isn't a setting: a file could use
// it to show a version and a link of its own as available, keep that answer with a matching tag, or
// stop the daily check with a time that never comes due. So a backup leaves it out, a restore deletes
// it, and a restored root with the check on asks again.
var updateCache = []string{"update.latest", "update.url", "update.etag", "update.checked-at"}

// ownSettings are read in this order. on and off are what each does, from the raw value or its absence,
// by the same rule as its reader: internal/upgrade's Checker.Enabled and runtime.SpeechMarking.
var ownSettings = []struct {
	key, label, byDefault string
	value                 func(raw string, ok bool) string
}{
	{"update.check", "Checks GitHub once a day for a newer version", "off", func(raw string, ok bool) string {
		if ok && raw == "on" {
			return "on"
		}
		return "off"
	}},
	{runtime.SpeechMarkingKey, "Marks synthesized speech as made by AI", "on", func(raw string, ok bool) string {
		if ok && raw == "off" {
			return "off"
		}
		return "on"
	}},
}

// OwnSetting is one setting that changes what the root does by itself, by what it does: on or off.
type OwnSetting struct {
	Setting string `json:"setting"`
	Value   string `json:"value"`
	// ThisRoot is what the root being replaced does. It's left out on a new root, and when it can't be read.
	ThisRoot           string `json:"thisRoot,omitempty"`
	ThisRootUnreadable bool   `json:"thisRootUnreadable,omitempty"`
}

// Text is the line as it reads in a report. On a new root, a value is set against what a new root does.
func (s OwnSetting) Text(newRoot bool) string {
	label, byDefault := s.Setting, ""
	for _, o := range ownSettings {
		if o.key == s.Setting {
			label, byDefault = o.label, o.byDefault
		}
	}
	line := label + ": " + s.Value
	switch {
	case s.ThisRootUnreadable:
		return line + " (this root's setting can't be read)"
	case newRoot && byDefault != "" && s.Value != byDefault:
		return line + " (a new root has it " + byDefault + ")"
	case !newRoot && s.ThisRoot != "" && s.ThisRoot != s.Value:
		return line + " (this root has it " + s.ThisRoot + ")"
	}
	return line
}

// markingOffCaution prints last, just before the question, because turning marking off makes the
// person answerable for telling people the speech was made by AI.
const markingOffCaution = `This turns off marking synthesized speech as made by AI. Telling people it is becomes your responsibility. See "What this root does by itself" above.`

// markingUnknownCaution is for a file that turns marking off over a root whose setting can't be read.
const markingUnknownCaution = `This may turn off marking synthesized speech as made by AI, since this root's setting can't be read. See "What this root does by itself" above.`

// readOwnSettings says what each setting does in a database. One with no settings table has the
// defaults, since a root last opened by an older version has none. A failed read is an error.
func readOwnSettings(ctx context.Context, db *sql.DB) (map[string]string, error) {
	present, err := tableExists(ctx, db, "go_settings")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, o := range ownSettings {
		raw, ok := "", false
		if present {
			var v sql.NullString
			err := db.QueryRowContext(ctx, `SELECT value FROM go_settings WHERE key = ?`, o.key).Scan(&v)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return nil, err
			default:
				raw, ok = v.String, true
			}
		}
		out[o.key] = o.value(raw, ok)
	}
	return out, nil
}

// liveOwnSettings reads the root being replaced. A failure is returned, and the report then says that
// root's setting can't be read.
func liveOwnSettings(ctx context.Context, liveDB string, liveErr error) (map[string]string, error) {
	if liveErr != nil {
		return nil, liveErr
	}
	db, err := openReadOnly(liveDB)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }() // read-only, so there's nothing a failed close could lose
	return readOwnSettings(ctx, db)
}

// ownSettingsReport sets what the backup does against what the root being replaced does. live is nil on
// a new root, with nothing to set it against. The caution is for marking turned off, here or maybe here.
func ownSettingsReport(backed, live map[string]string, liveErr error, newRoot bool) ([]OwnSetting, string) {
	var out []OwnSetting
	caution := ""
	for _, o := range ownSettings {
		s := OwnSetting{Setting: o.key, Value: backed[o.key]}
		thisRoot := o.byDefault
		switch {
		case newRoot:
		case liveErr != nil:
			s.ThisRootUnreadable = true
		default:
			s.ThisRoot, thisRoot = live[o.key], live[o.key]
		}
		if o.key == runtime.SpeechMarkingKey && s.Value == "off" {
			switch {
			case s.ThisRootUnreadable:
				caution = markingUnknownCaution
			case thisRoot == "on":
				caution = markingOffCaution
			}
		}
		out = append(out, s)
	}
	return out, caution
}
