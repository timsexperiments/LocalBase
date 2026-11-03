package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestExplainErrorChangesWhatAFailureSays(t *testing.T) {
	failing := errors.New("raw failure")
	app := func(run error, out, errw *bytes.Buffer) *App {
		return &App{Name: "x", Version: "1", Stdout: out, Stderr: errw,
			ExplainError: func(err error, c *Ctx) error {
				if errors.Is(err, failing) {
					return &ReasonedError{Err: errors.New("plain words for " + c.Global.Root), Reason: "named"}
				}
				return err
			},
			Root: &Command{Sub: []*Command{{Name: "go", Run: func(*Ctx) (any, error) { return nil, run }}}}}
	}
	var out, errw bytes.Buffer
	if code := app(failing, &out, &errw).Run([]string{"--root", "/r", "go"}); code != 1 {
		t.Errorf("exit %d", code)
	}
	if said := errw.String(); !strings.Contains(said, "plain words for /r") || !strings.Contains(said, "Reason: named") || strings.Contains(said, "raw failure") {
		t.Errorf("says: %s", said)
	}
	out.Reset()
	errw.Reset()
	if code := app(failing, &out, &errw).Run([]string{"--json", "go"}); code != 1 || !strings.Contains(out.String(), `"reason":"named"`) {
		t.Errorf("--json: %d %s", code, out.String())
	}
	// Anything else, and a mistake in the command line, keep their own words.
	out.Reset()
	errw.Reset()
	app(errors.New("another failure"), &out, &errw).Run([]string{"go"})
	if !strings.Contains(errw.String(), "another failure") {
		t.Errorf("says: %s", errw.String())
	}
	out.Reset()
	errw.Reset()
	if code := app(Inputf("bad input"), &out, &errw).Run([]string{"go"}); code == 1 || !strings.Contains(errw.String(), "bad input") {
		t.Errorf("input: %d %s", code, errw.String())
	}
}
