package main

import (
	"context"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// A command gets the host a test put in the App's Context, and this computer's without one.
func TestHostForTakesTheContextsHost(t *testing.T) {
	var got memory.HostProvider
	a := &cli.App{Name: "t", Root: &cli.Command{Sub: []*cli.Command{{Name: "x", Run: func(c *cli.Ctx) (any, error) {
		got = hostFor(c)
		return nil, nil
	}}}}}
	fixed := memory.HostProvider(fixedHost{real: memory.NewHostProvider()})
	a.Context = context.WithValue(context.Background(), testHostKey{}, fixed)
	if a.Run([]string{"x"}); got != fixed {
		t.Errorf("with a host in the Context: %#v", got)
	}
	a.Context = nil
	if a.Run([]string{"x"}); got == fixed || got == nil {
		t.Errorf("without one: %#v", got)
	}
	if _, fixedHere := hostFor(&cli.Ctx{}).(fixedHost); fixedHere {
		t.Error("a Ctx made without an App got the test host")
	}
}

// Upgrade gives a restarted gateway 90 s to answer ready unless a test set a shorter wait.
func TestReadyWaitLimit(t *testing.T) {
	if got := (&env{}).readyWaitLimit(); got != 90*time.Second {
		t.Errorf("unset: %v", got)
	}
	if got := (&env{readyWait: time.Second}).readyWaitLimit(); got != time.Second {
		t.Errorf("set: %v", got)
	}
}
