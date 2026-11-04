package backup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

// What these restore is small on disk and large once unpacked. Each measures what's allocated, which
// is what a file from someone else can run a computer out of, and not how long it takes.

const refusedAllocation = 32 << 20

// withFile sets a file in a backup, adding it if it isn't there, and reseals it.
func withFile(t *testing.T, entries []entry, name string, body []byte) []entry {
	t.Helper()
	for i := range entries {
		if entries[i].header.Name == filesPrefix+name {
			entries[i].body = body
			entries[i].header.Size = int64(len(body))
			reseal(t, entries)
			return entries
		}
	}
	for i := range entries {
		if strings.HasPrefix(entries[i].header.Name, filesPrefix) && entries[i].header.Typeflag == tar.TypeReg {
			h := *entries[i].header
			h.Name, h.Size = filesPrefix+name, int64(len(body))
			entries = append(entries, entry{&h, body})
			reseal(t, entries)
			return entries
		}
	}
	t.Fatal("no file to copy a header from")
	return nil
}

// allocatedByStage packs a backup and stages it, and says what that allocated and how it ended. It
// can't run beside another test, since the count is the whole process's.
func allocatedByStage(t *testing.T, entries []entry) (uint64, int64, error) {
	t.Helper()
	bad := filepath.Join(dir(t), "b"+Extension)
	pack(t, bad, entries)
	info, err := os.Stat(bad)
	if err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	s, stageErr := Stage(context.Background(), bad, filepath.Join(dir(t), "restored"), false)
	runtime.ReadMemStats(&after)
	if s != nil {
		s.Discard()
	}
	return after.TotalAlloc - before.TotalAlloc, info.Size(), stageErr
}

func refused(t *testing.T, err error, want string) {
	t.Helper()
	var r *Refused
	if !errors.As(err, &r) {
		t.Fatalf("not refused: %v", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("%q has no %q", err, want)
	}
}

// A file that lists more than a root could write is refused from its manifest, before anything is
// unpacked, so what it unpacks to doesn't matter.
func TestAFileThatUnpacksToALotAllocatesLittle(t *testing.T) {
	for _, c := range []struct {
		name, file, fill string
		size             int
	}{
		{"a list of former ids", formerLog, "\n", 256 << 20}, // a file of about 270 KB
		{"the access settings", "ui-access.json", " ", 64 << 20},
		{"the mail settings", "email-delivery.json", " ", 64 << 20},
		{"the sign-in profiles", "credentials/auth-profiles.json", " ", 64 << 20},
		{"a catalog entry", "catalog.d/big.json", " ", 64 << 20},
		{"a backend", "backends.d/big.json", " ", 64 << 20},
		{"a voice", "voices/" + voiceID + ".wav", " ", 64 << 20},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			e := withFile(t, read(t, create(t, r, false).Output), c.file, []byte(strings.Repeat(c.fill, c.size)))
			allocated, weighs, err := allocatedByStage(t, e)
			refused(t, err, "with a size a backup can't hold")
			if allocated > refusedAllocation {
				t.Errorf("a file of %d KB allocated %d MiB", weighs>>10, allocated>>20)
			}
		})
	}
}

// One past the limit is refused, and the limit itself isn't, so an honest file that size still restores.
func TestAFormerListAtItsLimit(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	e := withFile(t, read(t, create(t, r, false).Output), formerLog, []byte(strings.Repeat("\n", maxFormerBytes)))
	allocated, weighs, err := allocatedByStage(t, e)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if allocated > 64<<20 {
		t.Errorf("a file of %d KB allocated %d MiB", weighs>>10, allocated>>20)
	}
	e = withFile(t, e, formerLog, []byte(strings.Repeat("\n", maxFormerBytes+1)))
	_, _, err = allocatedByStage(t, e)
	refused(t, err, "with a size a backup can't hold")
}

func TestASettingsFileAtItsLimit(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	padded := func(n int) []byte { return []byte(strings.Repeat(" ", n)) }
	e := withFile(t, read(t, create(t, r, false).Output), "backends.d/big.json", padded(maxSettingsFileBytes))
	allocated, weighs, err := allocatedByStage(t, e)
	// It isn't a backend, so it's refused for that, and only after it's been read: the most this costs.
	refused(t, err, "not valid")
	if allocated > 128<<20 {
		t.Errorf("a file of %d KB allocated %d MiB", weighs>>10, allocated>>20)
	}
	e = withFile(t, e, "backends.d/big.json", padded(maxSettingsFileBytes+1))
	_, _, err = allocatedByStage(t, e)
	refused(t, err, "with a size a backup can't hold")
}

// Each file is within its limit, and together they aren't.
func TestManySettingsFilesAreLimitedTogether(t *testing.T) {
	m := &Manifest{Files: []File{{Path: "local-base.db", SHA256: strings.Repeat("0", 64)}}}
	for i := range maxSettingsTotalBytes/maxSettingsFileBytes + 1 {
		m.Files = append(m.Files, File{Path: fmt.Sprintf("catalog.d/%d.json", i), Size: maxSettingsFileBytes, SHA256: strings.Repeat("0", 64)})
	}
	_, _, err := checkManifestFiles(m)
	refused(t, err, "more settings than a backup can hold")
	m.Files = m.Files[:len(m.Files)-1]
	if _, _, err := checkManifestFiles(m); err != nil {
		t.Errorf("at the limit: %v", err)
	}
}

// The database and its recordings aren't settings: they don't count toward that, and each has its own.
func TestOtherFilesHaveTheirOwnLimits(t *testing.T) {
	sum := strings.Repeat("0", 64)
	voice := "voices/" + voiceID + ".wav"
	m := &Manifest{Files: []File{{Path: "local-base.db", Size: maxFileBytes, SHA256: sum}, {Path: voice, Size: 12 << 20, SHA256: sum}}}
	if _, _, err := checkManifestFiles(m); err != nil {
		t.Errorf("at their limits: %v", err)
	}
	full := &Manifest{Files: append([]File{}, m.Files[0])}
	for i := range voices.Limit {
		full.Files = append(full.Files, File{Path: fmt.Sprintf("voices/voice-%012d.wav", i), Size: voices.MaxUploadBytes, SHA256: sum})
	}
	if _, _, err := checkManifestFiles(full); err != nil {
		t.Errorf("as many voices as a root can have: %v", err)
	}
	m.Files[1].Size++
	_, _, err := checkManifestFiles(m)
	refused(t, err, "with a size a backup can't hold")
	m.Files[1].Size--
	m.Files[0].Size++
	_, _, err = checkManifestFiles(m)
	refused(t, err, "with a size a backup can't hold")
}

// A manifest that says less than a file holds is refused too, or the limit would be its word only.
func TestAFileLargerThanItsManifestSaysIsRefused(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	e := withFile(t, read(t, create(t, r, false).Output), formerLog, []byte("x"))
	m := manifestOf(t, e)
	for i := range e {
		if e[i].header.Name == filesPrefix+formerLog {
			e[i].body = []byte(strings.Repeat("\n", 64<<20))
			e[i].header.Size = int64(len(e[i].body))
		}
	}
	setManifest(t, e, m)
	allocated, _, err := allocatedByStage(t, e)
	refused(t, err, "not the size the manifest says")
	if allocated > refusedAllocation {
		t.Errorf("allocated %d MiB", allocated>>20)
	}
}

// What a database holds is compressed along with the rest. These are refused before any of it is read
// into memory.
func TestADatabaseHoldingALotAllocatesLittle(t *testing.T) {
	for _, c := range []struct {
		name string
		rows []string
		want string
	}{
		{"a key's name", []string{`UPDATE api_keys SET name = printf('%.*c', 67108864, 'x')`}, "a value of 67108864 bytes in api_keys"},
		{"a key's permissions", []string{`UPDATE api_keys SET scopes = printf('%.*c', 67108864, 'x')`}, "a value of 67108864 bytes in api_keys"},
		{"a value in usage", []string{`INSERT INTO usage_hourly VALUES (1, 'k', 'p', 'r', 'm', 200, 1, 1, 1, 1, 1, 1, printf('%.*c', 67108864, 'x'))`}, "a value of 67108864 bytes in usage_hourly"},
		{"too many rows", []string{
			`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('r', 'owner', 'x', 'x', 'x')`,
			fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < %d)
				INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) SELECT 'b'||i, 'r', 'i', 's'||i FROM n`, maxDatabaseRows+1)}, "100001 rows in auth_subject_role_bindings"},
		{"too much in one table", []string{
			fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 20)
				INSERT INTO api_keys (id, name, prefix, key_hash, created_at, last_rotated_at, scopes) SELECT 'k'||i, printf('%%.*c', %d, 'x'), 'p', 'h'||i, 'x', 'x', '[]' FROM n`, maxDatabaseValueBytes)}, "bytes in api_keys"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			e := read(t, create(t, r, false).Output)
			editDB(t, e, c.rows...)
			allocated, weighs, err := allocatedByStage(t, e)
			refused(t, err, c.want)
			if allocated > refusedAllocation {
				t.Errorf("a file of %d KB allocated %d MiB", weighs>>10, allocated>>20)
			}
		})
	}
}

// The limits are limits, and not a little below: at each, a restore goes on.
func TestADatabaseAtItsLimits(t *testing.T) {
	for _, c := range []struct {
		name string
		rows []string
	}{
		{"a value", []string{fmt.Sprintf(`UPDATE api_keys SET name = printf('%%.*c', %d, 'x')`, maxDatabaseValueBytes)}},
		{"rows", []string{
			`INSERT INTO auth_roles (id, name, description, created_at, updated_at) VALUES ('r', 'owner', 'x', 'x', 'x')`,
			fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < %d)
				INSERT INTO auth_subject_role_bindings (id, role_id, issuer, subject) SELECT 'b'||i, 'r', 'i', 's'||i FROM n`, maxDatabaseRows)}},
		{"usage piling up past the row limit", []string{
			fmt.Sprintf(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < %d)
				INSERT INTO usage_hourly SELECT i, 'k', 'p', 'r', 'm', 200, 1, 1, 1, 1, 1, 1, '[]' FROM n`, maxDatabaseRows+1)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, _ := newRoot(t, dir(t))
			e := read(t, create(t, r, false).Output)
			editDB(t, e, c.rows...)
			allocated, weighs, err := allocatedByStage(t, e)
			if err != nil {
				t.Fatalf("stage: %v", err)
			}
			if allocated > 256<<20 {
				t.Errorf("a file of %d KB allocated %d MiB", weighs>>10, allocated>>20)
			}
		})
	}
}

// A failed look is a refusal, not a pass. A closed database is the plainest way to fail one.
func TestAFailedLookAtASizeIsARefusal(t *testing.T) {
	st, err := store.Open(filepath.Join(dir(t), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	shape, err := store.ReadShape(context.Background(), st.DB)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	refused(t, checkDatabaseSize(context.Background(), st.DB, shape), "can't be read")
}

// A root's own file over its limit is said when backing up, since that backup could never be restored.
func TestBackingUpAFileOverItsLimitSaysSo(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	if err := os.WriteFile(filepath.Join(r.Dir, formerLog), []byte(strings.Repeat("\n", maxFormerBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Create(context.Background(), Options{Root: r, Output: filepath.Join(dir(t), "b"+Extension)})
	refused(t, err, "with a size a backup can't hold")
}

// An honest backup, with the real sizes of a small root, goes through all of this.
func TestAnHonestBackupAllocatesLittle(t *testing.T) {
	r, _ := newRoot(t, dir(t))
	allocated, _, err := allocatedByStage(t, read(t, create(t, r, false).Output))
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if allocated > refusedAllocation {
		t.Errorf("allocated %d MiB", allocated>>20)
	}
}
