package cli

import (
	"strings"
	"testing"
	"unicode"
)

func TestPrintable(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "",
		"plain text, with 'quotes'":  "plain text, with 'quotes'",
		"https://id.example.com/a?b": "https://id.example.com/a?b",
		"日本語 and émoji 🙂":            "日本語 and émoji 🙂",
		`a literal \x1b stays`:       `a literal \x1b stays`,
		"erase\x1b[2K":               `erase\x1b[2K`,
		"two\nlines":                 `two\x0alines`,
		"back\rover":                 `back\x0dover`,
		"tab\there":                  `tab\x09here`,
		"del\x7f":                    `del\x7f`,
		"nul\x00":                    `nul\x00`,
		"c1\u009b1A":                 `c1\x9b1A`,
		"not utf-8 \xff\x9b":         `not utf-8 \xff\x9b`,
		"reversed\u202egnp.exe":      `reversed\u202egnp.exe`,
		"isolate\u2066x":             `isolate\u2066x`,
		"hidden\u200bspace":          `hidden\u200bspace`,
		"line\u2028sep":              `line\u2028sep`,
		"para\u2029sep":              `para\u2029sep`,
	} {
		if got := Printable(in); got != want {
			t.Errorf("Printable(%q) = %q, want %q", in, got, want)
		}
	}
}

// Whatever goes in, nothing that acts on a terminal comes out.
func TestPrintableLeavesNothingThatActsOnATerminal(t *testing.T) {
	var all strings.Builder
	for r := rune(0); r <= 0x2ffff; r++ {
		if r < 0xd800 || r > 0xdfff {
			all.WriteRune(r)
		}
	}
	for _, r := range Printable(all.String()) {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			t.Fatalf("%U got through", r)
		}
	}
}
