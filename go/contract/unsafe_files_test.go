package contract

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// On the real binary: a link planted at the event log stops the gateway, naming the
// file and what to do, leaves the target alone, and doctor reports the same without
// starting anything. Removing it lets the gateway start.
func TestPlantedLogLinkRefusesToStart(t *testing.T) {
	if harness.Impl() != "go" && harness.Impl() != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := fsguard.ScratchDir(t)
	root := filepath.Join(tmp, "root")
	e := &e2e{t: t, root: root, outDir: tmp, noAuth: true}
	if r := e.cli(nil, "--non-interactive", "init"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	logs := filepath.Join(root, "logs")
	fsguard.MkdirAll(logs, 0o700)
	victim := filepath.Join(tmp, "victim")
	content := "someone's own file\n"
	fsguard.WriteFile(victim, []byte(content), 0o644)
	active := filepath.Join(logs, "events.jsonl")
	fsguard.RemoveAll(active)
	if err := fsguard.Symlink(victim, active); err != nil {
		t.Fatal(err)
	}
	args := []string{"--root", root, "serve", "--no-auth", "--no-llm", "--no-stt", "--no-tts", "--no-image", "--no-video", "--host", "127.0.0.1", "--port", strconv.Itoa(testutil.FreePort())}
	cmd := exec.Command(goCLI(t), args...)
	cmd.Env = cleanEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the gateway started: %s", out)
	}
	for _, want := range []string{active, "symbolic link", "Remove it", "who can write"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("no %q in: %s", want, out)
		}
	}
	if got, _ := os.ReadFile(victim); string(got) != content {
		t.Errorf("the victim changed: %q", got)
	}
	// A retry by a service manager: stderr says it again; the record does not move.
	again := exec.Command(goCLI(t), args...)
	again.Env = cleanEnv()
	if out, err := again.CombinedOutput(); err == nil || !strings.Contains(string(out), "Remove it") {
		t.Errorf("a retry: %v %s", err, out)
	}
	doctor := e.cli(nil, "--non-interactive", "doctor")
	if doctor.code != 0 || !strings.Contains(doctor.stderr+doctor.stdout, "Unsafe file") || !strings.Contains(doctor.stderr+doctor.stdout, active) {
		t.Errorf("doctor: %d %s %s", doctor.code, doctor.stdout, doctor.stderr)
	}
	asJSON := e.json("doctor")
	if list, _ := asJSON["unsafeFiles"].([]any); len(list) != 1 {
		t.Errorf("doctor --json: %v", asJSON["unsafeFiles"])
	}
	if asJSON["startRefusedSince"] == nil || !strings.Contains(doctor.stderr+doctor.stdout, "has been refusing to start since") {
		t.Errorf("doctor should say how long this has been going on: %v %s", asJSON["startRefusedSince"], doctor.stderr)
	}
	if got, _ := os.ReadFile(victim); string(got) != content {
		t.Errorf("doctor changed the victim: %q", got)
	}
	fsguard.RemoveLink(active)
	if clean := e.json("doctor"); clean["unsafeFiles"] != nil {
		t.Errorf("a clean root: %v", clean["unsafeFiles"])
	}
	e.serve("--no-auth", "--no-llm", "--no-stt", "--no-tts", "--no-image", "--no-video")
	defer e.stop()
	if r := e.do("GET", "/health", ""); r.Status != 200 {
		t.Errorf("after removing it: %d", r.Status)
	}
}
