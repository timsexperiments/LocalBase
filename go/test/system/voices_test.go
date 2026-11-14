package system

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// TestCLIVoices compares `voices list|add|remove` on twin roots.
func TestCLIVoices(t *testing.T) {
	pinned(t)
	ts, gr := twinRoots(t)
	roots := [2]string{ts, gr}
	for i, impl := range []string{"ts", "go"} {
		if r := runCLI(t, impl, roots[i], "--json", "init"); r.code != 0 {
			t.Fatalf("%s init: %s %s", impl, r.stdout, r.stderr)
		}
	}
	dir := fsguard.ScratchDir(t)
	clip := filepath.Join(dir, "clip.wav")
	fsguard.WriteFile(clip, harness.VoiceClip(11), 0o600)
	short := filepath.Join(dir, "short.wav")
	fsguard.WriteFile(short, harness.VoiceClip(11)[:44+16000*2], 0o600)
	volatile := func(p string) bool { return strings.HasSuffix(p, ".id") }
	for _, c := range []cliCase{
		{args: []string{"--json", "voices", "list"}},
		{args: []string{"--json", "voices", "add", clip, "--name", "Narrator"}},
		{args: []string{"--json", "voices", "add", filepath.Join(dir, "missing.wav"), "--name", "Narrator", "--consent"}},
		{args: []string{"--json", "voices", "add", dir, "--name", "Narrator", "--consent"}},
		{args: []string{"--json", "voices", "add", short, "--name", "Narrator", "--consent"}},
		{args: []string{"--json", "voices", "add", clip, "--name", "Harbor", "--consent"}},
		{args: []string{"--json", "voices", "add", clip, "--name", " Narrator ", "--consent"}, skip: volatile},
		{args: []string{"--json", "voices", "add", clip, "--name", "narrator", "--consent"}},
		{args: []string{"--json", "voices", "list"}, skip: volatile},
		{args: []string{"--json", "voices", "remove", "voice-000000000000"}},
		{args: []string{"--json", "voices", "remove", "../etc"}},
	} {
		compareCLI(t, roots, c)
	}
	// Human output and removal of each root's own voice.
	var lines [2]string
	for i, impl := range []string{"ts", "go"} {
		r := runCLI(t, impl, roots[i], "--json", "voices", "list")
		id := normalizeID(t, r.stdout)
		h := runCLI(t, impl, roots[i], "voices", "list")
		lines[i] = strings.ReplaceAll(volatileValue.ReplaceAllString(h.stdout, "<time>"), id, "<id>")
		if rm := runCLI(t, impl, roots[i], "--json", "voices", "remove", id); rm.code != 0 {
			t.Errorf("%s remove: %s %s", impl, rm.stdout, rm.stderr)
		}
		if _, err := os.Stat(filepath.Join(roots[i], "voices", id+".wav")); !os.IsNotExist(err) {
			t.Errorf("%s kept the recording: %v", impl, err)
		}
	}
	if lines[0] != lines[1] {
		t.Errorf("voices list output:\n ts: %q\n go: %q", lines[0], lines[1])
	}
	compareCLI(t, roots, cliCase{args: []string{"--json", "voices", "list"}})
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
