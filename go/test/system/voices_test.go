package system

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// TestCLIVoices pins `voices list|add|remove`.
func TestCLIVoices(t *testing.T) {
	pinned(t)
	gr := cliRoot(t)
	initRoot(t, gr)
	dir := fsguard.ScratchDir(t)
	clip := filepath.Join(dir, "clip.wav")
	fsguard.WriteFile(clip, harness.VoiceClip(11), 0o600)
	short := filepath.Join(dir, "short.wav")
	fsguard.WriteFile(short, harness.VoiceClip(11)[:44+16000*2], 0o600)
	for _, args := range [][]string{
		{"--json", "voices", "list"},
		{"--json", "voices", "add", clip, "--name", "Narrator"},
		{"--json", "voices", "add", filepath.Join(dir, "missing.wav"), "--name", "Narrator", "--consent"},
		{"--json", "voices", "add", dir, "--name", "Narrator", "--consent"},
		{"--json", "voices", "add", short, "--name", "Narrator", "--consent"},
		{"--json", "voices", "add", clip, "--name", "Harbor", "--consent"},
		{"--json", "voices", "add", clip, "--name", " Narrator ", "--consent"},
		{"--json", "voices", "add", clip, "--name", "narrator", "--consent"},
		{"--json", "voices", "list"},
		{"--json", "voices", "remove", "voice-000000000000"},
		{"--json", "voices", "remove", "../etc"},
	} {
		runCase(t, gr, args...)
	}
	// Human output and removal of the voice.
	r := runCLI(t, "go", gr, "--json", "voices", "list")
	id := normalizeID(t, r.stdout)
	runCase(t, gr, "voices", "list")
	if rm := runCLI(t, "go", gr, "--json", "voices", "remove", id); rm.code != 0 {
		t.Errorf("remove: %s %s", rm.stdout, rm.stderr)
	}
	if _, err := os.Stat(filepath.Join(gr, "voices", id+".wav")); !os.IsNotExist(err) {
		t.Errorf("kept the recording: %v", err)
	}
	runCase(t, gr, "--json", "voices", "list")
}

func normalizeID(t *testing.T, stdout string) string {
	t.Helper()
	var env struct {
		Data struct {
			Voices []struct{ ID string } `json:"voices"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || len(env.Data.Voices) != 1 {
		t.Fatalf("voices list: %v %s", err, stdout)
	}
	return env.Data.Voices[0].ID
}
