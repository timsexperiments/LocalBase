package cli

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Printable makes text from somewhere else safe to show on a terminal. A control character can move
// the cursor and erase what was printed, and a format character can reorder it, so each is shown as
// an escape instead. That way the person can see it was there. Everything else is unchanged.
func Printable(s string) string {
	if !strings.ContainsFunc(s, needsEscape) && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case needsEscape(r) && r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case needsEscape(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func needsEscape(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}
