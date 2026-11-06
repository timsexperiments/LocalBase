package gateway

import (
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// What a page reads from the preview is what a request then meets, to the byte, and asking
// loads nothing. Each figure gets its own gateway, as the controller's reading lasts a second.
func TestAdmissionPreviewAgreesWithALoadToTheByte(t *testing.T) {
	url := func(ts string) string { return ts + "/_localbase/models/" + chatModel + "/admission" }
	for _, total := range []int64{64 * memory.GiB, 640 * memory.GiB} { // the model decides, then a floor of a whole number of bytes
		ts, _, _ := newTestGateway(t, fixedHost{total, total})
		_, p := getJSON(t, url(ts.URL))
		req, reserve := int64(p["requestedBytes"].(float64)), int64(p["reserveBytes"].(float64))
		floor := (5*reserve + 3) / 4
		need := max(floor, req+reserve)
		if (need == floor) != (total == 640*memory.GiB) || (total == 640*memory.GiB && reserve%4 != 0) {
			t.Fatalf("%d total: requested %d, reserve %d, floor %d: not the case this means to test", total, req, reserve, floor)
		}
		for _, step := range []struct {
			avail int64
			fits  bool
		}{{need - 1, false}, {need, true}, {need + 1, true}} {
			ts, llm, _ := newTestGateway(t, fixedHost{total, step.avail})
			srv := ts.Config.Handler.(*Server)
			for range 3 {
				code, p := getJSON(t, url(ts.URL))
				if code != 200 || p["admitted"] != step.fits {
					t.Errorf("%d total, %d free (need %d): preview %d %v", total, step.avail, need, code, p)
				}
			}
			if id, _, _ := llm.Running(); id != "" {
				t.Errorf("the preview started %s", id)
			}
			if held := srv.Memory.Reserved(); len(held) != 0 {
				t.Errorf("the preview holds memory: %v", held)
			}
			r := post(t, ts, "/v1/chat/completions", chatBody("hi"))
			if (r.status == 200) != step.fits {
				t.Errorf("%d total, %d free (need %d): the load answered %d %.200s", total, step.avail, need, r.status, r.body)
			}
			if id, _, _ := llm.Running(); (id != "") != step.fits {
				t.Errorf("%d free (need %d): running %q after the request", step.avail, need, id)
			}
		}
	}
}
