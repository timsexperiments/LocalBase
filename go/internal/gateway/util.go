package gateway

import (
	"slices"
	"sort"
)

func contains(list []string, v string) bool { return slices.Contains(list, v) }

func sortStrings(s []string) { sort.Strings(s) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
