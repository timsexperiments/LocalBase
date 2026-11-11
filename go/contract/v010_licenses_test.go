package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// The root v0.1.0's installer really leaves: only licenses/, holding LICENSE and
// THIRD_PARTY_NOTICES.txt. Everyone who followed its instructions has this, and every
// command used to refuse it as nonempty.
func TestRootFromV010Installer(t *testing.T) {
	if harness.Impl() != "go" && harness.Impl() != "both" {
		t.Skip("runs the Go gateway")
	}
	for _, first := range [][]string{
		{"--non-interactive", "init"},
		{"--non-interactive", "configure", "--defaults"},
		{"--non-interactive", "models", "list"},
		{"--non-interactive", "doctor"},
	} {
		t.Run(strings.Join(first[1:], " "), func(t *testing.T) {
			tmp := fsguard.ScratchDir(t)
			e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true}
			licenses := filepath.Join(e.root, "licenses")
			fsguard.MkdirAll(licenses, 0o755)
			fsguard.WriteFile(filepath.Join(licenses, "LICENSE"), []byte("the license"), 0o644)
			fsguard.WriteFile(filepath.Join(licenses, "THIRD_PARTY_NOTICES.txt"), []byte("the notices"), 0o644)
			r := e.cli(nil, first...)
			if strings.Contains(r.stderr+r.stdout, "nonempty") {
				t.Fatalf("%v refused it: %s%s", first, r.stdout, r.stderr)
			}
			if r.code != 0 {
				t.Logf("%v exit %d: %s", first, r.code, r.stderr)
			}
			e.serve("--no-auth", "--no-llm", "--no-stt", "--no-tts", "--no-image", "--no-video")
			defer e.stop()
			if h := e.do("GET", "/health", ""); h.Status != 200 {
				t.Errorf("serve: %d", h.Status)
			}
			for name, want := range map[string]string{"LICENSE": "the license", "THIRD_PARTY_NOTICES.txt": "the notices"} {
				if got, err := os.ReadFile(filepath.Join(licenses, name)); err != nil || string(got) != want {
					t.Errorf("%s: %q %v", name, got, err)
				}
			}
		})
	}
}
