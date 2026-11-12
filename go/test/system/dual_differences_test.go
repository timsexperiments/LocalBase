package system

import (
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
