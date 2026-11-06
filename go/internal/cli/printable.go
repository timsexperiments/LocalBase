package cli

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
)

// Printable makes text from somewhere else safe to show on a terminal. A control character can move
// the cursor and erase what was printed, and a format character can reorder it, so each is shown as
// an escape instead. That way the person can see it was there. A newline shows as \n, since that's
// how anyone would write it. Everything else is unchanged, and text already escaped stays as it is.
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
		case r == '\n':
			b.WriteString(`\n`)
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

// PrintableASCII is Printable for a value a person judges by its letters, like an address. A letter
// from another alphabet can make one address look like another, so anything outside ASCII shows as
// an escape as well. A real internationalised address does too, which is the right trade where
// someone is deciding whether to trust it.
func PrintableASCII(s string) string {
	s = Printable(s)
	if !strings.ContainsFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r <= unicode.MaxASCII:
			b.WriteRune(r)
		case r <= 0xffff:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	return b.String()
}

// Lines is text whose line breaks are real, for a document this program encoded itself, like JSON or
// TOML, where a line break in a value is already written out. Each line is still escaped. Every use
// is named in TestEveryPrintedValueIsEscaped.
type Lines string

func (l Lines) printable() string {
	lines := strings.Split(string(l), "\n")
	for i, line := range lines {
		lines[i] = Printable(line)
	}
	return strings.Join(lines, "\n")
}

// shown escapes the values given to Info and Error. The format is this program's own, so it isn't.
func shown(args []any) []any {
	out := make([]any, len(args))
	for i, v := range args {
		out[i] = shownValue(v)
	}
	return out
}

func shownValue(v any) any {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || (rv.Kind() == reflect.Pointer && rv.IsNil()) {
		return v
	}
	switch x := v.(type) {
	case Lines:
		return x.printable()
	case error:
		return Printable(x.Error())
	case fmt.Stringer:
		return Printable(x.String())
	case []byte:
		return Printable(string(x))
	}
	switch rv.Kind() {
	case reflect.String:
		return Printable(rv.String())
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return v
	case reflect.Slice, reflect.Array:
		if k := rv.Type().Elem().Kind(); k != reflect.String && k != reflect.Interface && k != reflect.Pointer && k != reflect.Struct && k != reflect.Map && k != reflect.Slice {
			return v // numbers, for a verb like %d
		}
	}
	return Printable(fmt.Sprint(v))
}

// TextError is an error written by this program with line breaks of its own, and any value in it
// already escaped. Other errors print as one line.
type TextError struct{ Text string }

func (e *TextError) Error() string { return e.Text }

// shownMessage is what a person sees for err. raw is its text, and redacted what redaction left of it.
// Redaction removes one kind of escape sequence before anything else, and a path holding one would then
// look like a different path, so if that's all it did, raw is shown, escaped. If it did more, there was
// a secret or too much text, and what's shown is what it left. Lines are kept only where they're still
// this program's own.
func shownMessage(err error, raw, redacted string) string {
	message := redacted
	if obs.StripANSI(raw) == redacted {
		message = raw
	}
	var text *TextError
	if errors.As(err, &text) {
		if before, after, ok := strings.Cut(message, text.Text); ok {
			return Printable(before) + text.Text + Printable(after)
		}
	}
	return Printable(message)
}
