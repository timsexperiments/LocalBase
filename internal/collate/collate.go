// Package collate orders strings like JavaScript's String.localeCompare with
// the ICU root collation, which TypeScript LocalBase used for sorting names.
// ASCII follows the exact root order (punctuation is not ignorable, then
// symbols, digits, letters with lowercase first); other characters fall back
// to letters after digits, by code point.
package collate

import "unicode"

// order is the root collation order of printable ASCII, as Bun sorts it.
const order = " _-,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$0123456789aAbBcCdDeEfFgGhHiIjJkKlLmMnNoOpPqQrRsStTuUvVwWxXyYzZ"

var primary, tertiary [128]int

func init() {
	for i := range primary {
		primary[i] = -1
	}
	for i, c := range []byte(order) {
		if c >= 'A' && c <= 'Z' {
			primary[c] = primary[c+'a'-'A']
			tertiary[c] = 1
			continue
		}
		primary[c] = i
	}
}

func weight(r rune) (int, int) {
	if r < 128 && primary[r] >= 0 {
		return primary[r], tertiary[r]
	}
	switch {
	case unicode.IsLetter(r):
		l := unicode.ToLower(r)
		t := 0
		if l != r {
			t = 1
		}
		return 1000 + int(l), t
	case unicode.IsDigit(r):
		return 900, 0
	}
	return 0, 0
}

// Compare returns a negative, zero or positive number like localeCompare.
func Compare(a, b string) int {
	if c := compare(a, b, false); c != 0 {
		return c
	}
	return compare(a, b, true)
}

// Fold compares at base sensitivity: case is ignored.
func Fold(a, b string) int { return compare(a, b, false) }

func compare(a, b string, tert bool) int {
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		pa, ta := weight(ra[i])
		pb, tb := weight(rb[i])
		if tert {
			pa, pb = ta, tb
		}
		if pa != pb {
			return pa - pb
		}
	}
	return len(ra) - len(rb)
}
