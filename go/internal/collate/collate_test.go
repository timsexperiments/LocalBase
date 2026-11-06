package collate

import (
	"sort"
	"strings"
	"testing"
)

// Expected orders come from Bun's localeCompare.
func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"ab", "a-b", 1}, {"a b", "ab", -1}, {"a_b", "ab", -1}, {"a", "A", -1}, {"A", "b", -1},
		{`{"kind":"email-domain"}`, `{"kind":"email"}`, -1}, {"x1", "xa", -1}, {"abc", "ab", 1},
	}
	for _, c := range cases {
		got := Compare(c.a, c.b)
		if (got < 0) != (c.want < 0) || (got > 0) != (c.want > 0) {
			t.Errorf("Compare(%q, %q) = %d, want sign %d", c.a, c.b, got, c.want)
		}
	}
	chars := strings.Split(order, "")
	shuffled := append([]string(nil), chars...)
	sort.Strings(shuffled)
	sort.SliceStable(shuffled, func(i, j int) bool { return Compare(shuffled[i], shuffled[j]) < 0 })
	if strings.Join(shuffled, "") != order {
		t.Errorf("ASCII order %q", strings.Join(shuffled, ""))
	}
}
