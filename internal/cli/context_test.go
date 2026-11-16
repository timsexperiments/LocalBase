package cli

import (
	"context"
	"io"
	"testing"
)

type contextKey struct{}

// A command runs under the App's Context, and a Ctx made without one has context.Background().
func TestCtxContextIsTheApps(t *testing.T) {
	var got context.Context
	a := &App{Name: "t", Stdout: io.Discard, Stderr: io.Discard, Root: &Command{Sub: []*Command{{Name: "x", Run: func(c *Ctx) (any, error) {
		got = c.Context()
		return nil, nil
	}}}}}
	a.Context = context.WithValue(context.Background(), contextKey{}, "set")
	if a.Run([]string{"x"}); got == nil || got.Value(contextKey{}) != "set" {
		t.Errorf("with a Context: %v", got)
	}
	a.Context = nil
	if a.Run([]string{"x"}); got != context.Background() {
		t.Errorf("without one: %v", got)
	}
	if (&Ctx{}).Context() != context.Background() {
		t.Error("a Ctx made by hand")
	}
}
