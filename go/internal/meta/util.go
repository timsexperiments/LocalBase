package meta

import (
	"encoding/json"
	"sort"

	"github.com/timsexperiments/LocalBase/go/internal/collate"
)

func jsonUnmarshal(b []byte, v any) { _ = json.Unmarshal(b, v) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// localeCompare is JavaScript's String.localeCompare.
func localeCompare(a, b string) int { return collate.Compare(a, b) }

// caseFold is localeCompare at base sensitivity: case is ignored entirely.
func caseFold(a, b string) int { return collate.Fold(a, b) }
