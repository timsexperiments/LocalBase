package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Values are escaped wherever Info and Error are given one, and the format is left alone.
func TestInfoAndErrorEscapeValuesOnly(t *testing.T) {
	const hostile = "x\x1b[2K\ny"
	const want = `x\x1b[2K\ny`
	type named string
	for name, tc := range map[string]struct {
		format string
		arg    any
		out    string
	}{
		"string":      {"a\tb %s", hostile, "a\tb " + want},
		"named type":  {"%s", named(hostile), want},
		"error":       {"%v", errors.New(hostile), want},
		"stringer":    {"%s", stringer(hostile), want},
		"bytes":       {"%s", []byte(hostile), want},
		"strings":     {"%v", []string{hostile}, "[" + want + "]"},
		"in an any":   {"%v", any(hostile), want},
		"map":         {"%v", map[string]string{"k": hostile}, "map[k:" + want + "]"},
		"number":      {"%d", 42, "42"},
		"numbers":     {"%d", []int{1, 2}, "[1 2]"},
		"nil":         {"%v", nil, "<nil>"},
		"lines":       {"%s", Lines("one\x1b[2K\ntwo"), "one\\x1b[2K\ntwo"},
		"plain stays": {"%s", "日本語 and a written \\x1b", "日本語 and a written \\x1b"},
	} {
		var stdout, stderr bytes.Buffer
		c := &Ctx{Stdout: &stdout, Stderr: &stderr}
		c.Info(tc.format, tc.arg)
		c.Error(tc.format, tc.arg)
		c.Out(tc.format, tc.arg)
		if got := stderr.String(); got != tc.out+"\n" {
			t.Errorf("%s: Error wrote %q, want %q", name, got, tc.out+"\n")
		}
		if got := stdout.String(); got != tc.out+"\n"+tc.out+"\n" {
			t.Errorf("%s: Info and Out wrote %q", name, got)
		}
	}
}

type stringer string

func (s stringer) String() string { return string(s) }

// An error can't fake a line of the output, unless this program wrote the line.
func TestFailedCommandsPrintOneEscapedMessage(t *testing.T) {
	const hostile = "x\x1b[2K\nFAKE"
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"plain":                        {errors.New(hostile), `Error: x\x1b[2K\nFAKE`},
		"input":                        {Inputf("%s", hostile), `Error: x\x1b[2K\nFAKE`},
		"text":                         {&TextError{Text: "one\n  two"}, "Error: one\n  two"},
		"a sequence redaction removes": {errors.New("at /a\x1b[2Kb"), `Error: at /a\x1b[2Kb`},
		"osc":                          {errors.New("at /a\x1b]0;x\x07b"), `Error: at /a\x1b]0;x\x07b`},
		"bare esc":                     {errors.New("at /a\x1bb"), `Error: at /a\x1bb`},
		"c1":                           {errors.New("at /a\u009b1Ab"), `Error: at /a\x9b1Ab`},
		"a secret stays redacted":      {errors.New("x\x1b[2Ky token=abcdef"), `Error: xy token=[REDACTED]`},
		"wrapped":                      {fmt.Errorf("from %s: %w", hostile, &TextError{Text: "one\ntwo"}), `Error: from x\x1b[2K\nFAKE: one` + "\ntwo"},
	} {
		var stdout, stderr bytes.Buffer
		a := &App{Name: "t", Stdout: &stdout, Stderr: &stderr, Root: &Command{Name: "t", Sub: []*Command{{
			Name: "go", Run: func(*Ctx) (any, error) { return nil, tc.err },
		}}}}
		a.Run([]string{"go"})
		if got := strings.TrimRight(stderr.String(), "\n"); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

// A script comparing the message in the envelope still gets it as it was.
func TestTheEnvelopeKeepsTheMessage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := &App{Name: "t", Stdout: &stdout, Stderr: &stderr, Root: &Command{Name: "t", Sub: []*Command{{
		Name: "go", Run: func(*Ctx) (any, error) { return nil, errors.New("a\nb") },
	}}}}
	a.Run([]string{"--json", "go"})
	if !strings.Contains(stdout.String(), `"a\nb"`) {
		t.Errorf("envelope: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), `Error: a\nb`) {
		t.Errorf("stderr: %q", stderr.String())
	}
}
