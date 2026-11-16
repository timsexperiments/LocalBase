package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCountVoices(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i, id := range []string{"", "a", "b"} {
		if id != "" {
			if _, err := st.DB.Exec(`INSERT INTO tts_custom_voices (id, name, name_key, sha256, size_bytes, duration_ms, created_at, created_by, consent_statement, consent_attested_at)
				VALUES (?, ?, ?, ?, 1, 1, 't', 'cli', 'I consent.', 't')`, "voice_"+id, "Name "+id, "name "+id, strings.Repeat(id, 64)); err != nil {
				t.Fatal(err)
			}
		}
		if n, err := st.CountVoices(ctx); err != nil || n != i {
			t.Errorf("after %d: %d, %v", i, n, err)
		}
	}
	listed, err := st.Voices(ctx)
	if n, _ := st.CountVoices(ctx); err != nil || n != len(listed) {
		t.Errorf("count %d, listed %d (%v)", n, len(listed), err)
	}
}
