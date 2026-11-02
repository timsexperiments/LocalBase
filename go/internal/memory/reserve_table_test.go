package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

// The tables in docs/memory-reserve.md, computed through the real admission and
// pressure code on a fake host, so the document cannot drift from the code. Only
// the first policy is the shipped default; the others are proposals.

type tableHost struct {
	total, avail int64
}

func (h *tableHost) Topology() Topology {
	return Topology{Unified: true, SystemCapacity: h.total, GPUWorkingSetBytes: appleWorkingSetHeuristic(h.total)}
}
func (h *tableHost) Snapshot(context.Context) Snapshot {
	return Snapshot{Available: h.avail, Pressure: "normal"}
}

// clampReserve is the proposed form: a share of memory between a floor and a cap
// (cap 0 means none).
func clampReserve(total int64, percent, floorGiB, capGiB float64) int64 {
	r := math.Max(float64(total)*percent/100, floorGiB*float64(GiB))
	if capGiB > 0 {
		r = math.Min(r, capGiB*float64(GiB))
	}
	return int64(r)
}

var reservePolicies = []struct {
	heading string
	reserve func(total int64) int64
}{
	{"Today: 15%, at least 8 GiB", func(t int64) int64 { return Reserve{Percent: 15, MinimumGB: 8}.Bytes(t) }},
	{"Option A: 10%, at least 2 GiB, at most 8 GiB", func(t int64) int64 { return clampReserve(t, 10, 2, 8) }},
	{"Option B: 10%, at least 3 GiB, at most 8 GiB", func(t int64) int64 { return clampReserve(t, 10, 3, 8) }},
	{"Option C: 10%, at least 3 GiB, no cap", func(t int64) int64 { return clampReserve(t, 10, 3, 0) }},
}

func gibs(b int64) string { return fmt.Sprintf("%.1f", float64(b)/float64(GiB)) }

// reserveAsPolicy expresses an exact byte reserve in the settings' terms.
func reserveAsPolicy(b int64) Reserve { return Reserve{MinimumGB: float64(b) / float64(GiB)} }

func TestReserveTables(t *testing.T) {
	doc, err := os.ReadFile("../../docs/memory-reserve.md")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, p := range reservePolicies {
		_, section, found := strings.Cut(string(doc), "### "+p.heading+"\n")
		if !found {
			t.Errorf("the document has no table for %q", p.heading)
		}
		section, _, _ = strings.Cut(section, "\n#")
		for _, totalGiB := range []int64{8, 16, 24, 32, 64, 128} {
			total := totalGiB * GiB
			reserve := p.reserve(total)
			warn := int64(math.Ceil(float64(reserve) * warningReserveMultiplier))
			largest := max(0, min(total-reserve, appleWorkingSetHeuristic(total)))
			host := &tableHost{total: total}
			c := NewController(host, reserveAsPolicy(reserve), false)
			c.SetSampler(nil)
			check := func(model, avail int64) error {
				host.avail = avail
				return c.CheckAdmission(ctx, Demand{UnifiedBytes: model, AcceleratorBytes: model, Confidence: "derived"}, nil)
			}
			permanent := func(err error) bool {
				var ae *AdmissionError
				return errors.As(err, &ae) && ae.Permanent
			}
			row := fmt.Sprintf("| %d | %s | %s |", totalGiB, gibs(reserve), gibs(warn))
			for _, modelGiB := range []int64{1, 4, 8} {
				model := modelGiB * GiB
				if model > largest {
					row += " can never load |"
					if err := check(model, total); !permanent(err) {
						t.Errorf("%s, %d GiB host, %d GiB model should be permanent: %v", p.heading, totalGiB, modelGiB, err)
					}
					continue
				}
				need := max(warn, model+reserve)
				row += " " + gibs(need) + " |"
				if err := check(model, need); err != nil {
					t.Errorf("%s, %d GiB host, %d GiB model at %s free: %v", p.heading, totalGiB, modelGiB, gibs(need), err)
				}
				if err := check(model, need-MiB); err == nil || permanent(err) {
					t.Errorf("%s, %d GiB host, %d GiB model just under: %v", p.heading, totalGiB, modelGiB, err)
				}
			}
			row += " " + gibs(largest) + " |"
			if largest > 0 {
				if err := check(largest, total); err != nil {
					t.Errorf("%s, %d GiB host, the largest model: %v", p.heading, totalGiB, err)
				}
			}
			if err := check(largest+MiB, total); !permanent(err) {
				t.Errorf("%s, %d GiB host, just over the largest: %v", p.heading, totalGiB, err)
			}
			if !strings.Contains(section, row) {
				t.Errorf("%s: the document lacks the row\n%s", p.heading, row)
			}
			// The pressure lines move with the reserve.
			for _, c2 := range []struct {
				avail  int64
				action string
			}{{warn, "allow"}, {warn - MiB, "constrain"}, {reserve - MiB, "emergency-stop"}} {
				if c2.avail < 0 {
					continue
				}
				host.avail = c2.avail
				fresh := NewController(host, reserveAsPolicy(reserve), false)
				if got := fresh.Poll(ctx).Action; got != c2.action {
					t.Errorf("%s, %d GiB host at %s free: %s, want %s", p.heading, totalGiB, gibs(c2.avail), got, c2.action)
				}
			}
		}
	}
}

// The case that prompted this: a 24 GiB model on a 64 GiB Mac with 31 GiB free.
func TestReserveUserCase(t *testing.T) {
	ctx := context.Background()
	total, model, avail := 64*GiB, 24*GiB, 31*GiB
	run := func(reserve int64) error {
		c := NewController(&tableHost{total: total, avail: avail}, reserveAsPolicy(reserve), false)
		c.SetSampler(nil)
		return c.CheckAdmission(ctx, Demand{UnifiedBytes: model, AcceleratorBytes: model, Confidence: "derived"}, nil)
	}
	if err := run(Reserve{Percent: 15, MinimumGB: 8}.Bytes(total)); err == nil {
		t.Error("today's default should refuse it")
	}
	if err := run(clampReserve(total, 10, 3, 8)); err != nil {
		t.Errorf("the proposal should admit it: %v", err)
	}
}
