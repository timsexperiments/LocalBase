package system

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Where the Go and TS command lines differ on purpose. The dual tests (-impl=both) compare the
// two, and each difference they allow is named here with its reason. Each is also asserted where
// it is used: the difference must be there, so a change on either side shows up as a failure
// instead of being masked.

// goOnlyUninstallReadCopies is uninstall's count of the read copies of the database it removed.
// Go's CLI reads a copy of a root's database while a gateway owns its WAL (#647, the integration
// tree's third build; cmd/localbase/read_copies_test.go), and uninstall removes those copies. TS
// has no read copies, so its answer has no such field.
const goOnlyUninstallReadCopies = ".data.readCopiesRemoved"

// jsonAt is the value at a DiffJSON path (".data.field") in a decoded JSON document.
func jsonAt(v any, path string) (any, bool) {
	for _, key := range strings.Split(strings.TrimPrefix(path, "."), ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[key]; !ok {
			return nil, false
		}
	}
	return v, true
}

// tsClearNeedsProvider is TS's refusal of `access policy clear` when no sign-in provider is
// configured. Go's CLI, like the gateway's clear-policy, needs no provider
// to clear: clearing only ever falls back to the provider-wide permissions, and with no policy Go
// says it was already clear (exit 0, cleared false). Asserted at the end of TestCLIAccess.
const tsClearNeedsProvider = "Configure a browser identity provider first."

// tsCannotReadGoRoot is TS's answer when it reads a root Go made. Go's store is WAL (#647), and TS
// opens the database read-only (src/db/client.ts openReadOnlyDatabase), which SQLite refuses for a
// WAL file with no -shm: "unable to open database file". Go is the product and TS only the
// reference, so a TS that can't read Go's roots is the expected outcome; Go still reads TS's.
const tsCannotReadGoRoot = `{"ok":false,"error":{"code":"operational_error","message":"unable to open database file"}}`

// goReadCopyNotice is the line Go prints on stderr each time a command reads a copy of the root's
// database rather than the database itself (#647; `config apply` reads twice and prints it twice).
// TS reads in place and prints nothing.
const goReadCopyNotice = "Read a copy of this root's database, as it stood just now.\n"

// goOnlyRootFiles are the files only Go's store makes in a root: the record of which process owns
// the WAL (#647). TS's roots have none.
var goOnlyRootFiles = []string{"local-base.db-wal-owner"}

// readCopyNotices takes every goReadCopyNotice out of Go's stderr before it is compared with TS's,
// and counts them. A test that uses it checks the count with expectSeen: the notice must
// have appeared, so a Go that stops printing it fails instead of leaving a stale exception.
type readCopyNotices struct{ seen int }

func (n *readCopyNotices) strip(goStderr string) string {
	if !strings.Contains(goStderr, goReadCopyNotice) {
		return goStderr
	}
	n.seen += strings.Count(goStderr, goReadCopyNotice)
	return strings.ReplaceAll(goStderr, goReadCopyNotice, "")
}

func (n *readCopyNotices) expectSeen(t testing.TB) {
	t.Helper()
	if n.seen == 0 {
		t.Errorf("Go never printed its read-copy notice here, so goReadCopyNotice is a stale difference")
	}
}

// tsMemoryRefusal marks TS's answer when its admission refuses a runtime start for memory. On
// macOS, TS counts only free and inactive pages as available
// (src/domains/runtime/memory/macos-memory-provider.ts), which a busy CI runner can leave short
// even with 80% of memory free, where Go, measuring differently, admits the same request (go-dual
// run 43010301192). Admission runs after the permission check (a refused role gets 403 first), so
// the answer still shows that TS let the request through the permission check.
const tsMemoryRefusal = `"code":"insufficient_memory"`

// tsAdmissionRejected is the event TS writes to the root's log, with its measured inputs, when it
// refuses for memory.
const tsAdmissionRejected = "runtime.memory-admission-rejected"

// lastLogLine is the last line of the log at path that holds marker, for a test's own log.
func lastLogLine(path, marker string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(%v)", err)
	}
	lines := strings.Split(string(b), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], marker) {
			return lines[i]
		}
	}
	return fmt.Sprintf("(no %s in %s)", marker, path)
}
