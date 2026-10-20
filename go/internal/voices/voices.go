// Package voices stores custom speech voice profiles: a consented reference
// recording under <root>/voices, addressed only by a server-issued id, and
// its `tts_custom_voices` row (the TS voice store, #366).
package voices

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/third_party/norm"
)

const (
	// MinSeconds is the shortest reference clip; the speaker encoder needs a
	// few seconds of speech.
	MinSeconds = 3
	// MaxSeconds is the longest reference clip.
	MaxSeconds = 30
	// MaxUploadBytes fits 30 s of 48 kHz stereo 32-bit audio plus headers.
	MaxUploadBytes = 12 * 1024 * 1024
	// Limit caps stored profiles; each is resident in the warm runtime.
	Limit = 32
	// SampleRateHz is what stored references are normalized to.
	SampleRateHz = 24_000

	ConsentStatementID = "voice-consent-v1"
	ConsentStatement   = "I have the right to use this voice: it is my own, or its speaker has given permission for it to be cloned here."
)

// BuiltIn are the voice names catalog runtimes provide; never usable as
// custom names.
var BuiltIn = []string{"default", "harbor", "willow", "af_heart", "af_bella"}

// IDPattern matches custom voice ids.
var IDPattern = regexp.MustCompile(`^voice-[0-9a-f]{12}$`)

// Error is a rejected voice operation; messages never quote the audio.
type Error struct{ Code, Message string }

func (e *Error) Error() string { return e.Message }

func notFound() error { return &Error{Code: "voice_not_found", Message: "Voice not found."} }

var nameTaken = &Error{Code: "voice_name_taken", Message: "Another voice already uses that name."}

// Voice is a stored profile, in the TS response field order.
type Voice struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	DurationMs int64   `json:"durationMs"`
	SizeBytes  int64   `json:"sizeBytes"`
	SHA256     string  `json:"sha256"`
	CreatedAt  string  `json:"createdAt"`
	Consent    Consent `json:"consent"`
	// CreatedBy is present only for callers that can manage voices.
	CreatedBy string `json:"createdBy,omitempty"`
}

type Consent struct {
	Statement  string `json:"statement"`
	AttestedAt string `json:"attestedAt"`
}

// Public is the profile without its creator.
func (v Voice) Public() Voice { v.CreatedBy = ""; return v }

// isJSSpace is JavaScript's \s and String.prototype.trim set.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// ParseName applies the TS name schema: NFC, trimmed, inner whitespace
// collapsed, 1–48 UTF-16 units, no control or format characters, not a
// built-in voice name and not shaped like a voice id. The error is the
// first failing rule's message.
func ParseName(name string) (string, error) {
	name = norm.NFC.String(name)
	name = strings.Join(strings.FieldsFunc(name, isJSSpace), " ")
	fail := func(msg string) (string, error) { return "", &Error{Code: "invalid_voice_name", Message: msg} }
	switch {
	case name == "":
		return fail("Voice name is required.")
	case utf16Len(name) > 48:
		return fail("Voice name must be at most 48 characters.")
	case strings.ContainsFunc(name, func(r rune) bool { return unicode.In(r, unicode.Cc, unicode.Cf) }):
		return fail("Voice name cannot contain control characters.")
	case slices.Contains(BuiltIn, NameKey(name)):
		return fail("That name belongs to a built-in voice.")
	case IDPattern.MatchString(name):
		return fail("Voice name cannot look like a voice id.")
	}
	return name, nil
}

// NameKey is the uniqueness key: NFKC, then JavaScript's toLowerCase.
func NameKey(name string) string {
	return jsLower(norm.NFKC.String(name))
}

// jsLower is String.prototype.toLowerCase: per-rune lowercase plus the two
// mappings it applies that unicode.ToLower does not (İ → i̇, final sigma).
func jsLower(s string) string {
	rs := []rune(s)
	cased := func(r rune) bool { return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.IsTitle(r) }
	ignorable := func(r rune) bool {
		return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) ||
			strings.ContainsRune("'.:·’‘․﹒＇．", r)
	}
	var b strings.Builder
	for i, r := range rs {
		switch {
		case r == 0x130:
			b.WriteString("i̇")
			continue
		case r == 0x3a3:
			before, after := false, false
			for j := i - 1; j >= 0; j-- {
				if !ignorable(rs[j]) {
					before = cased(rs[j])
					break
				}
			}
			for j := i + 1; j < len(rs); j++ {
				if !ignorable(rs[j]) {
					after = cased(rs[j])
					break
				}
			}
			if before && !after {
				b.WriteRune(0x3c2)
				continue
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// Store is the voice store of one root.
type Store struct {
	db   *sql.DB
	root string
	mu   sync.Mutex // serializes the name and limit checks with their insert
}

func New(st *store.Store, rootDir string) *Store { return &Store{db: st.DB, root: rootDir} }

// Dir is the private directory of reference clips; never served directly.
func Dir(rootDir string) string { return filepath.Join(rootDir, "voices") }

// Path builds a reference path from a validated id only, never user text.
func Path(rootDir, id string) (string, bool) {
	if !IDPattern.MatchString(id) {
		return "", false
	}
	return filepath.Join(Dir(rootDir), id+".wav"), true
}

const columns = `id, name, duration_ms, size_bytes, sha256, created_at, created_by, consent_attested_at`

func scan(row interface{ Scan(...any) error }) (Voice, error) {
	var v Voice
	err := row.Scan(&v.ID, &v.Name, &v.DurationMs, &v.SizeBytes, &v.SHA256, &v.CreatedAt, &v.CreatedBy, &v.Consent.AttestedAt)
	v.Consent.Statement = ConsentStatementID
	return v, err
}

// List returns every profile, oldest first.
func (s *Store) List(ctx context.Context) ([]Voice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+columns+` FROM tts_custom_voices ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Voice{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Get returns a profile, or nil.
func (s *Store) Get(ctx context.Context, id string) (*Voice, error) {
	if !IDPattern.MatchString(id) {
		return nil, nil
	}
	v, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM tts_custom_voices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *Store) fileMatches(v *Voice) string {
	p, ok := Path(s.root, v.ID)
	if !ok {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != v.SHA256 {
		return ""
	}
	return p
}

// Resolve is the reference path of a stored voice whose file still matches
// its checksum ("" otherwise).
func (s *Store) Resolve(ctx context.Context, id string) (string, error) {
	v, err := s.Get(ctx, id)
	if err != nil || v == nil {
		return "", err
	}
	return s.fileMatches(v), nil
}

// ReferenceFiles maps every stored voice whose file matches to its path.
func (s *Store) ReferenceFiles(ctx context.Context) (map[string]string, error) {
	list, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range list {
		if p := s.fileMatches(&list[i]); p != "" {
			out[list[i].ID] = p
		}
	}
	return out, nil
}

func (s *Store) nameAvailable(ctx context.Context, name, except string) error {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM tts_custom_voices WHERE name_key = ?`, NameKey(name)).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	case id != except:
		return nameTaken
	}
	return nil
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED")
}

// ensurePrivateDir creates the owner-only voice directory and refuses a
// planted symlink.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("the custom voice directory is not a directory")
	}
	if info.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Create validates and stores a profile. consent must be true: the creator
// attests they may use this voice.
func (s *Store) Create(ctx context.Context, name string, consent bool, audio []byte, createdBy string) (*Voice, error) {
	if !consent {
		return nil, &Error{Code: "consent_required", Message: "Confirm that you have permission to use this voice."}
	}
	name, err := ParseName(name)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.nameAvailable(ctx, name, ""); err != nil {
		return nil, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tts_custom_voices`).Scan(&total); err != nil {
		return nil, err
	}
	if total >= Limit {
		return nil, &Error{Code: "voice_limit_reached", Message: fmt.Sprintf("At most %d custom voices can be stored. Delete one first.", Limit)}
	}
	n, err := NormalizeAudio(audio)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(n.WAV)
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	v := &Voice{ID: "voice-" + randomHex(6), Name: name, DurationMs: n.DurationMs, SizeBytes: int64(len(n.WAV)),
		SHA256: hex.EncodeToString(sum[:]), CreatedAt: now, CreatedBy: createdBy,
		Consent: Consent{Statement: ConsentStatementID, AttestedAt: now}}
	if err := ensurePrivateDir(Dir(s.root)); err != nil {
		return nil, err
	}
	path, _ := Path(s.root, v.ID)
	tmp := path + "." + randomHex(4) + ".partial"
	err = os.WriteFile(tmp, n.WAV, 0o600)
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err == nil {
		_, err = s.db.ExecContext(ctx, `INSERT INTO tts_custom_voices (id, name, name_key, sha256, size_bytes, duration_ms,
			created_at, created_by, consent_statement, consent_attested_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			v.ID, v.Name, NameKey(v.Name), v.SHA256, v.SizeBytes, v.DurationMs, now, createdBy, ConsentStatementID, now)
	}
	if err != nil {
		os.Remove(tmp)
		os.Remove(path)
		if isUniqueViolation(err) {
			return nil, nameTaken
		}
		return nil, err
	}
	return v, nil
}

// Rename changes a profile's display name.
func (s *Store) Rename(ctx context.Context, id, name string) (*Voice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, notFound()
	}
	if name, err = ParseName(name); err != nil {
		return nil, err
	}
	if err := s.nameAvailable(ctx, name, id); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tts_custom_voices SET name = ?, name_key = ? WHERE id = ?`, name, NameKey(name), id); err != nil {
		if isUniqueViolation(err) {
			return nil, nameTaken
		}
		return nil, err
	}
	v.Name = name
	return v, nil
}

// Remove deletes a profile and its recording. The row goes first, so a
// request racing the delete fails cleanly.
func (s *Store) Remove(ctx context.Context, id string) (*Voice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, notFound()
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tts_custom_voices WHERE id = ?`, id); err != nil {
		return nil, err
	}
	path, _ := Path(s.root, id)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return v, nil
}
