package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type nvidiaHost struct{ *fakeHost }

func (nvidiaHost) NVIDIAPresent() bool { return true }

// A refusal says whether its host has an NVIDIA GPU: what a reporting host says, else NVIDIAPresent.
// The field never reaches JSON.
func TestARefusalCarriesItsHostsNVIDIAAnswer(t *testing.T) {
	refusal := func(h HostProvider) *AdmissionError {
		t.Helper()
		// Not wrapped: a wrapper that embeds HostProvider would hide NVIDIAPresent. Nothing is
		// resident, so nothing is sampled.
		c := NewController(h, Reserve{Percent: 15, MinimumGB: 8}, false)
		var ae *AdmissionError
		if err := c.CheckAdmission(context.Background(), demand(100), nil); !errors.As(err, &ae) {
			t.Fatalf("not refused: %v", err)
		}
		return ae
	}
	_, h := newTestController(10)
	if !refusal(nvidiaHost{h}).NVIDIAPresent {
		t.Error("a host with an NVIDIA GPU: not stamped")
	}
	if got := refusal(h).NVIDIAPresent; got != NVIDIAPresent() {
		t.Errorf("a host that doesn't say: %v, NVIDIAPresent says %v", got, NVIDIAPresent())
	}
	b, err := json.Marshal(&AdmissionError{Reason: "system-memory", NVIDIAPresent: true})
	if err != nil || strings.Contains(strings.ToLower(string(b)), "nvidia") {
		t.Errorf("the field reached JSON: %s %v", b, err)
	}
}
