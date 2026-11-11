package cli

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
)

// Lines is text whose line breaks are real, for a document this program encoded itself, like JSON or
// TOML, where a line break in a value is already written out. Each line is still escaped. Every use
// is named in TestEveryPrintedValueIsEscaped.
type Lines string

func (l Lines) printable() string {
	lines := strings.Split(string(l), "\n")
	for i, line := range lines {
		lines[i] = termsafe.Printable(line)
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
		return termsafe.Printable(x.Error())
	case fmt.Stringer:
		return termsafe.Printable(x.String())
	case []byte:
		return termsafe.Printable(string(x))
	}
	switch rv.Kind() {
	case reflect.String:
		return termsafe.Printable(rv.String())
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return v
	case reflect.Slice, reflect.Array:
		if k := rv.Type().Elem().Kind(); k != reflect.String && k != reflect.Interface && k != reflect.Pointer && k != reflect.Struct && k != reflect.Map && k != reflect.Slice {
			return v // numbers, for a verb like %d
		}
	}
	return termsafe.Printable(fmt.Sprint(v))
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
			return termsafe.Printable(before) + text.Text + termsafe.Printable(after)
		}
	}
	return termsafe.Printable(message)
}
