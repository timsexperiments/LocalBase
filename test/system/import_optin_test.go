package system

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// On the host, the opt-in to private sources works per import and as a setting, and
// it can be turned off again. Local paths need no opt-in here.
func TestImportOptInFromTheCLI(t *testing.T) {
	weights := ggufFile(t, "qwen3", nil, 2048)
	srv := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "m.gguf", time.Time{}, strings.NewReader(string(weights)))
	}), false)
	tmp := fsguard.ScratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	n := 0
	importFrom := func(extra ...string) cliResult {
		n++
		args := append([]string{"--json", "models", "import", srv.URL + "/m.gguf", "--allow-http", "--task", "chat", "--id", "m" + strconv.Itoa(n), "--yes"}, extra...)
		return e.cli(nil, args...)
	}
	if r := importFrom(); r.code == 0 || !strings.Contains(r.stdout+r.stderr, "allow-private-imports") {
		t.Fatalf("without the opt-in: %d %s %s", r.code, r.stdout, r.stderr)
	}
	if r := importFrom("--allow-private"); r.code != 0 {
		t.Fatalf("--allow-private: %s %s", r.stdout, r.stderr)
	}
	if r := importFrom(); r.code == 0 {
		t.Fatalf("--allow-private lasted past its import: %s", r.stdout)
	}
	if r := e.cli(nil, "--json", "configure", "--defaults", "--no-create-key", "--allow-private-imports"); r.code != 0 {
		t.Fatalf("configure: %s %s", r.stdout, r.stderr)
	}
	if r := importFrom(); r.code != 0 {
		t.Fatalf("with the setting: %s %s", r.stdout, r.stderr)
	}
	if r := e.cli(nil, "--json", "configure", "--defaults", "--no-create-key", "--no-allow-private-imports"); r.code != 0 {
		t.Fatalf("configure off: %s %s", r.stdout, r.stderr)
	}
	if r := importFrom(); r.code == 0 {
		t.Fatalf("the setting stayed on: %s", r.stdout)
	}
	// The metadata range stays refused, however the opt-in is given.
	if r := e.cli(nil, "--json", "models", "import", "http://169.254.169.254/m.gguf", "--allow-http", "--task", "chat", "--id", "meta", "--yes", "--allow-private"); r.code == 0 || !strings.Contains(r.stdout+r.stderr, "does not download") {
		t.Fatalf("metadata: %d %s %s", r.code, r.stdout, r.stderr)
	}
	// A file on this machine stays a CLI import.
	local := filepath.Join(tmp, "local.gguf")
	fsguard.WriteFile(local, weights, 0o600)
	if r := e.cli(nil, "--json", "models", "import", local, "--task", "chat", "--id", "local"); r.code != 0 {
		t.Fatalf("local path: %s %s", r.stdout, r.stderr)
	}
}
