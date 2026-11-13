package gateway

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// Whether a loaded model's next load would differ is one answer: the placement page and the CLI's
// placement-state give the same one, and both count a change of the configured context ceiling as
// well as of the placement. The CLI used to work it out for itself, without the ceiling.
func TestPendingNextLoadIsOneAnswer(t *testing.T) {
	testPlacement = &runtime.HostPlacement{DeviceID: "test-device", Platform: placement.Discrete,
		GPUs: func() []memory.GPUMemory {
			return []memory.GPUMemory{{Index: 0, Name: "GPU", FreeBytes: 0, TotalBytes: 12 << 30}}
		}}
	defer func() { testPlacement = nil }()
	h := &steppedHost{}
	h.avail.Store(60 * memory.GiB)
	ts, llm, _ := newTestGateway(t, h)
	cat, _ := catalog.LoadWithFormer()
	spec := cat.ByID(gatewaytest.ChatModel)
	placementtest.WriteMoEGGUF(t, filepath.Join(llm.Config().Root, "models", "llm", spec.Primary().Filename), spec.Primary().ExpectedSizeBytes)
	s := runServer(ts)
	release, err := s.cfg.Root.Lock("127.0.0.1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if code, _ := placementCall(t, "POST", ts.URL+"/v1/chat/completions", `{"model":"`+gatewaytest.ChatModel+`","messages":[{"role":"user","content":"hi"}]}`); code != 200 {
		t.Fatalf("chat: %d", code)
	}
	page := func() any {
		t.Helper()
		_, m := placementCall(t, "GET", ts.URL+"/_localbase/models/"+gatewaytest.ChatModel+"/placement", "")
		return m["pendingNextLoad"]
	}
	cli := func() any {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/_localbase/placement-state?model="+gatewaytest.ChatModel, nil)
		req.Header.Set("X-Localbase-Instance-Token", s.cfg.Root.Instance().InstanceToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return m["pendingNextLoad"]
	}
	check := func(what string, want bool) {
		t.Helper()
		if p, c := page(), cli(); p != want || c != want {
			t.Errorf("%s: the page says %v, the CLI's placement-state %v; want %v", what, p, c, want)
		}
	}
	check("as loaded", false)
	cfg := llm.Config().Clone()
	was := cfg.CtxSize
	cfg.CtxSize = was + 4096
	llm.SetConfig(cfg)
	check("after a context change", true)
	cfg = llm.Config().Clone()
	cfg.CtxSize = was
	llm.SetConfig(cfg)
	check("the context back", false)
	placementCall(t, "PUT", ts.URL+"/_localbase/models/"+gatewaytest.ChatModel+"/placement", `{"mode":"cpu"}`)
	check("after a placement change", true)
}
