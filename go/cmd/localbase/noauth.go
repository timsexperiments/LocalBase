package main

import (
	"errors"
	"net"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/netguard"
)

// noAuthHostError refuses a --no-auth serve that would bind beyond loopback (netguard.NoAuthHost),
// as bad input.
func noAuthHostError(host string) error { return noAuthInput(netguard.NoAuthHost(host)) }

// noAuthListenerError is the same check on the address really bound (netguard.NoAuthListener).
func noAuthListenerError(addr net.Addr) error { return noAuthInput(netguard.NoAuthListener(addr)) }

// noAuthInput answers a --no-auth refusal as bad input, in its own words.
func noAuthInput(err error) error {
	var r *netguard.NoAuthRefusal
	if errors.As(err, &r) {
		return cli.Inputf("%s", r.Message)
	}
	return err
}
