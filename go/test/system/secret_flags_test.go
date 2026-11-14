package system

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// A value on a command line can be read by anyone on this computer while the command runs, and stays
// in the shell's history. configure says so, and says what to do instead, without repeating the value.
func TestASecretOnTheCommandLineGetsAWarning(t *testing.T) {
	const planted = "planted-secret-value"
	for _, c := range []struct {
		name string
		args []string
		warn string
	}{
		{"a Hugging Face token", []string{"--hf-token", planted}, "--hf-token"},
		{"OTLP headers", []string{"--otel-headers", "x-auth=" + planted}, "--otel-headers"},
		{"clearing the token", []string{"--hf-token", ""}, ""},
		{"neither", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := filepath.Join(fsguard.ScratchDir(t), "root")
			if r := runCLI(t, "go", root, "--json", "init"); r.code != 0 {
				t.Fatalf("init: %s %s", r.stdout, r.stderr)
			}
			r := runCLI(t, "go", root, append([]string{"--json", "configure", "--defaults", "--no-create-key"}, c.args...)...)
			if r.code != 0 {
				t.Fatalf("configure: %d %s %s", r.code, r.stdout, r.stderr)
			}
			if strings.Contains(r.stdout+r.stderr, planted) {
				t.Error("the output repeats the value")
			}
			warned := strings.Contains(r.stderr, "can be read by other users")
			if warned != (c.warn != "") {
				t.Errorf("warned: %v: %s", warned, r.stderr)
			}
			if c.warn != "" && !strings.Contains(r.stderr, c.warn) {
				t.Errorf("it doesn't say which: %s", r.stderr)
			}
		})
	}
}
