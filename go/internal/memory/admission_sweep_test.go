package memory

import (
	"context"
	"math"
	"testing"
)

// Over everything a host might have free, admission flips once, from refused to admitted, at the
// demand plus the reserve, and a larger model never gets in where a smaller one doesn't. A refusal
// says what it was about. Each figure is checked either side of the boundary.
func TestAdmissionFlipsOnceAtTheBoundaryForEachSize(t *testing.T) {
	// A fresh controller each time: a low reading would otherwise leave it in its pressure state, and
	// this is about the figures.
	at := func(avail int64) (*Controller, *fakeHost) {
		c, host := newTestController(0)
		host.avail = avail
		return c, host
	}
	c, host := at(0)
	reserve := c.system.Bytes(host.topo.SystemCapacity)
	boundaryOf := func(d Demand) int64 {
		// Below 1.25 times the reserve it is pressure, whatever the demand.
		return max(c.projected(d)+reserve, int64(math.Ceil(float64(reserve)*warningReserveMultiplier)))
	}
	small, large := demand(1), demand(12)
	boundaries := map[string]int64{}
	for name, d := range map[string]Demand{"small": small, "large": large} {
		boundary := boundaryOf(d)
		boundaries[name] = boundary
		admittedBefore := false
		flips := 0
		for avail := int64(0); avail <= host.topo.SystemCapacity; avail += GiB / 4 {
			c, _ := at(avail)
			err := c.CheckAdmission(context.Background(), d, nil)
			admitted := err == nil
			if admitted != admittedBefore {
				flips++
				admittedBefore = admitted
			}
			if admitted != (avail >= boundary) {
				t.Errorf("%s at %d free: admitted %v, boundary %d (%v)", name, avail, admitted, boundary, err)
			}
			if !admitted && reason(err) == "" {
				t.Errorf("%s at %d free: a refusal with no reason: %v", name, avail, err)
			}
		}
		if flips != 1 {
			t.Errorf("%s flipped %d times over the sweep", name, flips)
		}
		// Exactly at the boundary, and just under.
		c, _ := at(boundary)
		if err := c.CheckAdmission(context.Background(), d, nil); err != nil {
			t.Errorf("%s at its boundary: %v", name, err)
		}
		c, _ = at(boundary - 1)
		if err := c.CheckAdmission(context.Background(), d, nil); err == nil {
			t.Errorf("%s just under its boundary was admitted", name)
		}
	}
	if boundaries["large"] <= boundaries["small"] {
		t.Errorf("the larger model's boundary %d isn't above the smaller's %d", boundaries["large"], boundaries["small"])
	}
}
