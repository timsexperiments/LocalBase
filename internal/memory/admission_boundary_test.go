package memory

import (
	"context"
	"testing"
)

// admissionRoutes are the three ways admission is asked: a real load's reservation, a switch's
// preflight, and the preview a page decides from. They answer the same at any figure.
func admissionRoutes(t *testing.T, total, avail, model int64) (reserve, check, preview error) {
	t.Helper()
	fresh := func() *Controller {
		c := NewController(unsampled{&tableHost{total: total, avail: avail}}, Reserve{Percent: 15, MinimumGB: 8}, false)
		return c
	}
	d := Demand{UnifiedBytes: model, AcceleratorBytes: model, Confidence: "measured"}
	ctx := context.Background()
	if r, err := fresh().Reserve(ctx, "m", d); err != nil {
		reserve = err
	} else {
		r.Release()
	}
	check = fresh().CheckAdmission(ctx, d, nil)
	preview = fresh().Preview(ctx, d, nil).Err
	return reserve, check, preview
}

// To the byte: what admission lets in is max(ceil(1.25 x reserve), model + reserve). A model
// small beside the reserve meets the floor first, including a floor that isn't a whole number.
func TestAdmissionAtTheExactBoundary(t *testing.T) {
	for _, tc := range []struct {
		name         string
		total, model int64
		floorDecides bool
	}{
		{"model and reserve decide", 80 * GiB, 10 * GiB, false},
		{"the pressure floor decides, a whole number", 80 * GiB, GiB, true},
		{"the pressure floor decides, a fraction", 64 * GiB, GiB, true},
		{"both decide: model and reserve are exactly the floor", 80 * GiB, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Reserve{Percent: 15, MinimumGB: 8}.Bytes(tc.total)
			floor := (5*r + 3) / 4 // ceil(1.25 x r), in whole bytes
			if whole := r%4 == 0; whole != (tc.total == 80*GiB) {
				t.Fatalf("reserve %d: this case means the floor to be whole on 80 GiB, and a fraction on 64", r)
			}
			if tc.model < 0 {
				tc.model = floor - r
			}
			need := max(floor, tc.model+r)
			if (need == floor) != tc.floorDecides {
				t.Fatalf("reserve %d, floor %d, need %d: not the case this names", r, floor, need)
			}
			for _, step := range []struct {
				avail int64
				fits  bool
			}{{need - 1, false}, {need, true}, {need + 1, true}} {
				res, check, prev := admissionRoutes(t, tc.total, step.avail, tc.model)
				for name, err := range map[string]error{"Reserve": res, "CheckAdmission": check, "Preview": prev} {
					if (err == nil) != step.fits {
						t.Errorf("%s at %d free (need %d): %v", name, step.avail, need, err)
					}
				}
			}
		})
	}
}
