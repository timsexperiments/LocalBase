package structuredoutput

import (
	"encoding/json"
	"slices"
	"sort"
)

// Verbatim copies of three gateway helpers, so this package needs only the standard library. A later
// step folds each pair into one.

// Twin of gateway's contains (internal/gateway/util.go).
func contains(list []string, v string) bool { return slices.Contains(list, v) }

// Twin of gateway's sortedKeys (internal/gateway/util.go).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Twin of gateway's number (internal/gateway/validate.go).
// number reads a JSON number from a UseNumber decode.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	}
	return 0, false
}
