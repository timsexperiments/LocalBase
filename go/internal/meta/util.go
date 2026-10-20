package meta

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"
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

// localeCompare approximates JavaScript's String.localeCompare (ICU root
// collation) for catalog names: case-insensitive first, with whitespace and
// punctuation before digits before letters; case breaks remaining ties,
// lowercase first.
func localeCompare(a, b string) int {
	class := func(r rune) int {
		switch {
		case unicode.IsLetter(r):
			return 3
		case unicode.IsDigit(r):
			return 2
		}
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		x, y := unicode.ToLower(ra[i]), unicode.ToLower(rb[i])
		if cx, cy := class(x), class(y); cx != cy {
			return cx - cy
		}
		if x != y {
			return int(x) - int(y)
		}
	}
	if len(ra) != len(rb) {
		return len(ra) - len(rb)
	}
	for i := range ra {
		if ra[i] != rb[i] {
			if unicode.IsLower(ra[i]) {
				return -1
			}
			return 1
		}
	}
	return 0
}

// caseFold is localeCompare at base sensitivity: case is ignored entirely.
func caseFold(a, b string) int {
	if c := localeCompare(strings.ToLower(a), strings.ToLower(b)); c != 0 {
		return c
	}
	return 0
}
