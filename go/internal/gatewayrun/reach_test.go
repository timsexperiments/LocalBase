package gatewayrun

import (
	"os/exec"
	"strings"
	"testing"
)

// A process loses its locks on the database when it closes any descriptor on that file, whatever
// opened it, and the gateway's lock is what tells another process it's attached
// (internal/store/held_file.go). internal/backup opens and closes the files it hashes, the database
// among them, so nothing the gateway runs may reach it. cmd/localbase's
// TestServeNeverReachesInternalBackup follows serve's functions inside the command line, which does
// use backup; Run's package holds none of it, so here the check is the whole import graph, for the
// default and the localbase_lab builds.
func TestRunNeverReachesInternalBackup(t *testing.T) {
	const module = "github.com/timsexperiments/LocalBase/go/internal/"
	for _, tags := range [][]string{nil, {"-tags", "localbase_lab"}} {
		args := append(append([]string{"list", "-deps"}, tags...), ".")
		out, err := exec.Command("go", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		deps := strings.Fields(string(out))
		seen := map[string]bool{}
		for _, p := range deps {
			seen[p] = true
			if p == module+"backup" || strings.HasPrefix(p, module+"backup/") {
				t.Errorf("%v: gatewayrun imports %s", tags, p)
			}
		}
		// The check means nothing if the list isn't the gateway's.
		for _, want := range []string{module + "gateway", module + "store", module + "runtime"} {
			if !seen[want] {
				t.Errorf("%v: %s is not among gatewayrun's dependencies (%d listed): update this test", tags, want, len(deps))
			}
		}
	}
}
