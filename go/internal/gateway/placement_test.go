package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// placementGateway is a test gateway on a discrete GPU of free bytes, with
// the chat model's file a 2-layer MoE GGUF header.
func placementGateway(t *testing.T, platform string, free int64) (string, *runtime.LLM, int64) {
	t.Helper()
	testPlacement = &runtime.HostPlacement{DeviceID: "test-device", Platform: platform,
		GPUs: func() []memory.GPUMemory {
			return []memory.GPUMemory{{Index: 0, Name: "GPU", FreeBytes: free, TotalBytes: 12 << 30}}
		}}
	defer func() { testPlacement = nil }()
	h := &steppedHost{}
	h.avail.Store(60 * memory.GiB)
	ts, llm, _ := newTestGateway(t, h)
	cat, _ := catalog.LoadWithFormer()
	spec := cat.ByID(chatModel)
	path := filepath.Join(llm.Config().Root, "models", "llm", spec.Primary().Filename)
	placementtest.WriteMoEGGUF(t, path, spec.Primary().ExpectedSizeBytes)
	return ts.URL, llm, spec.Primary().ExpectedSizeBytes
}

func placementCall(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// The placement API (DESIGN §20.2): store, validate, preview, apply on the
// next load only, and refuse while the model loads.
func TestPlacementAPI(t *testing.T) {
	base, llm, _ := placementGateway(t, placement.Discrete, 0)
	url := base + "/_localbase/models/" + chatModel + "/placement"

	code, m := placementCall(t, "GET", url, "")
	if code != 200 || m["requested"].(map[string]any)["mode"] != "auto" || m["loaded"] != false {
		t.Fatalf("GET: %d %v", code, m)
	}
	// GPU-only cannot fit a GPU with no free memory: 409 with the numbers.
	code, m = placementCall(t, "PUT", url, `{"mode":"gpu"}`)
	if code != 409 || errCode(m) != "placement_does_not_fit" || !strings.Contains(m["error"].(map[string]any)["message"].(string), "free") {
		t.Errorf("gpu-only on a full GPU: %d %v", code, m)
	}
	// Unknown fields for the mode and bad values: 400.
	if code, m = placementCall(t, "PUT", url, `{"mode":"auto","gpuLayers":3}`); code != 400 || errCode(m) != "placement_unsupported" {
		t.Errorf("gpuLayers with auto: %d %v", code, m)
	}
	if code, m = placementCall(t, "PUT", url, `{"mode":"custom","cpuMoeLayers":9}`); code != 400 || errCode(m) != "placement_unsupported" {
		t.Errorf("9 expert layers on a 2-layer model: %d %v", code, m)
	}
	// CPU-only fits system memory; stored and shown as the next load.
	code, m = placementCall(t, "PUT", url, `{"mode":"cpu"}`)
	if code != 200 || m["requested"].(map[string]any)["mode"] != "cpu" {
		t.Fatalf("cpu: %d %v", code, m)
	}
	next, _ := m["next"].(map[string]any)
	if next == nil || next["gpuLayers"] != 0.0 || next["platform"] != placement.Discrete {
		t.Errorf("next load: %v", m)
	}
	// The context size is the shared setting, saved as explicit.
	code, m = placementCall(t, "PUT", url, `{"mode":"cpu","contextSize":16384}`)
	if code != 200 || llm.Config().CtxSize != 16384 || llm.Config().CtxMode != "explicit" || m["requested"].(map[string]any)["contextScope"] != "all models" {
		t.Errorf("context: %d %v (config %d %s)", code, m, llm.Config().CtxSize, llm.Config().CtxMode)
	}
	// DELETE: back to auto.
	if code, m = placementCall(t, "DELETE", url, ""); code != 200 || m["requested"].(map[string]any)["mode"] != "auto" {
		t.Errorf("DELETE: %d %v", code, m)
	}
	if code, _ = placementCall(t, "PUT", base+"/_localbase/models/no-such-model/placement", `{"mode":"cpu"}`); code != 404 {
		t.Errorf("unknown model: %d", code)
	}
}

// Loaded models keep their split until the next load; a change shows as
// pending and never restarts the runtime. Changes are refused mid-load.
func TestPlacementAppliesOnNextLoad(t *testing.T) {
	base, llm, _ := placementGateway(t, placement.Discrete, 0)
	url := base + "/_localbase/models/" + chatModel + "/placement"
	placementCall(t, "PUT", url, `{"mode":"cpu"}`)
	t.Setenv("FAKE_STARTUP_MS", "1500")
	done := make(chan int)
	go func() {
		code, _ := placementCall(t, "POST", base+"/v1/chat/completions", `{"model":"`+chatModel+`","messages":[{"role":"user","content":"hi"}]}`)
		done <- code
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !llm.Loading(chatModel) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if code, m := placementCall(t, "PUT", url, `{"mode":"auto"}`); code != 409 || errCode(m) != "model_loading" {
		t.Errorf("PUT mid-load: %d %v", code, m)
	}
	if code, m := placementCall(t, "DELETE", url, ""); code != 409 || errCode(m) != "model_loading" {
		t.Errorf("DELETE mid-load: %d %v", code, m)
	}
	if code := <-done; code != 200 {
		t.Fatalf("chat: %d", code)
	}
	_, m := placementCall(t, "GET", url, "")
	loaded, _ := m["resolved"].(map[string]any)
	if m["loaded"] != true || loaded == nil || loaded["gpuLayers"] != 0.0 || loaded["plannedAt"] == "" || m["pendingNextLoad"] != false {
		t.Fatalf("loaded: %v", m)
	}
	// A change on the loaded model is stored and pending; the backend keeps
	// its plan and is not restarted.
	before, beforePlan, _ := llm.Running()
	code, m := placementCall(t, "PUT", url, `{"mode":"auto"}`)
	if code != 200 || m["pendingNextLoad"] != true {
		t.Fatalf("change while loaded: %d %v", code, m)
	}
	after, afterPlan, _ := llm.Running()
	if before != after || beforePlan != afterPlan || afterPlan.Placement.GPULayers != 0 {
		t.Errorf("the runtime changed without a reload: %v %+v", after, afterPlan.Placement)
	}
}

// /_localbase/models/{id} carries device.placement: the next load's split
// (preview) until the model loads.
func TestModelMetadataPlacement(t *testing.T) {
	base, _, _ := placementGateway(t, placement.Discrete, 0)
	code, m := placementCall(t, "GET", base+"/_localbase/models/"+chatModel, "")
	dev, _ := m["device"].(map[string]any)
	p, _ := dev["placement"].(map[string]any)
	if code != 200 || p == nil || p["preview"] != true || p["platform"] != placement.Discrete || p["note"] == "" {
		t.Errorf("%d device %v", code, dev)
	}
}
