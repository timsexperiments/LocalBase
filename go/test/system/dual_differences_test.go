package system

import "strings"

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
