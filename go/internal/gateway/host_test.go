package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// fixedHost has no GPU limit and a set amount free.
type fixedHost struct{ total, avail int64 }

func (h fixedHost) Topology() memory.Topology {
	return memory.Topology{Unified: true, SystemCapacity: h.total}
}
func (h fixedHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: h.avail, Pressure: "normal"}
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

func hostMem(m map[string]any) map[string]any {
	return m["host"].(map[string]any)["memory"].(map[string]any)
}

func bytesOf(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(map[string]any)["bytes"].(float64)
	if !ok {
		t.Fatalf("%s has no bytes: %v", key, m[key])
	}
	return v
}

// Every resident runtime counts, each labelled, and other is what remains.
func TestHostMemorySplit(t *testing.T) {
	ts, _, mem := newTestGateway(t, fixedHost{64 * memory.GiB, 40 * memory.GiB})
	mem.SetSampler(func(context.Context, int) (int64, bool) { return 3 * memory.GiB, true })
	if r := post(t, ts, "/v1/chat/completions", gatewaytest.ChatBody("hi")); r.status != 200 {
		t.Fatalf("chat: %d %s", r.status, r.body)
	}
	res, err := mem.Reserve(context.Background(), "image", memory.Demand{UnifiedBytes: 2 * memory.GiB, HostBytes: 2 * memory.GiB, Confidence: "derived"})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Release()
	time.Sleep(1100 * time.Millisecond) // past the shared reading
	code, m := getJSON(t, ts.URL+"/_localbase/host")
	if code != 200 {
		t.Fatalf("%d %v", code, m)
	}
	h := hostMem(m)
	gib := float64(memory.GiB)
	if bytesOf(t, h, "total") != 64*gib || bytesOf(t, h, "available") != 40*gib || h["total"].(map[string]any)["source"] != "system" {
		t.Errorf("total and available: %v", h)
	}
	lb := h["localBase"].(map[string]any)
	if lb["bytes"].(float64) != 5*gib || lb["source"] != "mixed" {
		t.Errorf("localBase: %v", lb)
	}
	rts := lb["runtimes"].([]any)
	if len(rts) != 2 {
		t.Fatalf("runtimes: %v", rts)
	}
	img, llm := rts[0].(map[string]any), rts[1].(map[string]any)
	if img["runtime"] != "image" || img["source"] != "planned" || img["measuredBytes"] != nil || img["bytes"].(float64) != 2*gib {
		t.Errorf("image: %v", img)
	}
	if llm["runtime"] != "llm" || llm["source"] != "measured" || llm["bytes"].(float64) != 3*gib || llm["modelId"] != gatewaytest.ChatModel {
		t.Errorf("llm: %v", llm)
	}
	if bytesOf(t, h, "other") != (64-40-5)*gib || h["other"].(map[string]any)["source"] != "derived" {
		t.Errorf("other: %v", h["other"])
	}
	if h["pressure"] != "healthy" {
		t.Errorf("pressure: %v", h["pressure"])
	}
}

func TestHostOtherClampsAtZero(t *testing.T) {
	ts, _, mem := newTestGateway(t, fixedHost{64 * memory.GiB, 63 * memory.GiB})
	res, err := mem.Reserve(context.Background(), "image", memory.Demand{UnifiedBytes: 10 * memory.GiB, HostBytes: 10 * memory.GiB, Confidence: "derived"})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Release()
	_, m := getJSON(t, ts.URL+"/_localbase/host")
	if v := hostMem(m)["other"].(map[string]any); v["bytes"] != 0.0 {
		t.Errorf("other: %v", v)
	}
}

type unknownHost struct{}

func (unknownHost) Topology() memory.Topology { return memory.Topology{Unified: true} }
func (unknownHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: -1, Pressure: "unknown"}
}

// What is not known is null, never a zero that claims nothing is in use.
func TestHostUnknownIsNull(t *testing.T) {
	ts, _, _ := newTestGateway(t, unknownHost{})
	_, m := getJSON(t, ts.URL+"/_localbase/host")
	h := hostMem(m)
	for _, key := range []string{"total", "available", "other"} {
		v := h[key].(map[string]any)
		if b, present := v["bytes"]; !present || b != nil || v["source"] != "unavailable" {
			t.Errorf("%s: %v", key, v)
		}
	}
	if p, present := h["pressure"]; !present || p != nil {
		t.Errorf("pressure: %v", p)
	}
}

type countingHost struct {
	fixedHost
	reads *atomic.Int64
}

func (h countingHost) Snapshot(ctx context.Context) memory.Snapshot {
	h.reads.Add(1)
	return h.fixedHost.Snapshot(ctx)
}

// Many pages polling share one sample a second: no process per call.
func TestHostReadsAreCached(t *testing.T) {
	var reads, samples atomic.Int64
	ts, _, mem := newTestGateway(t, countingHost{fixedHost{64 * memory.GiB, 50 * memory.GiB}, &reads})
	mem.SetSampler(func(context.Context, int) (int64, bool) { samples.Add(1); return memory.GiB, true })
	if r := post(t, ts, "/v1/chat/completions", gatewaytest.ChatBody("hi")); r.status != 200 {
		t.Fatalf("chat: %d", r.status)
	}
	time.Sleep(1100 * time.Millisecond)
	reads.Store(0)
	samples.Store(0)
	for range 30 {
		if code, _ := getJSON(t, ts.URL+"/_localbase/host"); code != 200 {
			t.Fatal(code)
		}
		getJSON(t, ts.URL+"/_localbase/models/"+gatewaytest.ChatModel+"/admission")
	}
	if r, s := reads.Load(), samples.Load(); r > 3 || s > 3 {
		t.Errorf("60 reads sampled the host %d times and footprints %d times", r, s)
	}
}

func TestHostActivity(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	activity := func() map[string]any {
		_, m := getJSON(t, ts.URL+"/_localbase/host")
		return m["host"].(map[string]any)["activity"].(map[string]any)
	}
	a := activity()
	if a["busy"] != false || a["llm"].(map[string]any)["configured"] != true || a["image"].(map[string]any)["configured"] != false {
		t.Errorf("idle: %v", a)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		post(t, ts, "/v1/chat/completions", `{"model":"`+gatewaytest.ChatModel+`","stream":true,"messages":[{"role":"user","content":"fake:slow 30"}]}`)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		a = activity()
		if a["llm"].(map[string]any)["inFlight"] == 1.0 && a["busy"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never busy: %v", a)
		}
		time.Sleep(20 * time.Millisecond)
	}
	<-done
	// A detached run counts even with nothing yet holding the model.
	c := gatewaytest.RunClient{T: t, Base: ts.URL}
	code, run := c.Do("POST", "/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("fake:slow 20"))
	if code != 202 {
		t.Fatalf("run: %d %v", code, run)
	}
	a = activity()
	if a["runs"].(map[string]any)["active"] != 1.0 || a["busy"] != true {
		t.Errorf("with a run: %v", a)
	}
	c.WaitStatus(run["id"].(string), "completed")
	if a = activity(); a["runs"].(map[string]any)["active"] != 0.0 {
		t.Errorf("after the run: %v", a)
	}
}

// The preview and the real refusal are one path: same status, code and message,
// same requested figure as the real rejection logged, and nothing starts.
func TestAdmissionPreviewMatchesRefusal(t *testing.T) {
	for name, h := range map[string]memory.HostProvider{
		"can never fit":   fixedHost{8 * memory.GiB, 7 * memory.GiB},
		"low right now":   fixedHost{64 * memory.GiB, 10 * memory.GiB},
		"GPU can not fit": smallHost{},
	} {
		t.Run(name, func(t *testing.T) {
			logs := &lockedBuffer{}
			gatewaytest.Log = logs
			ts, llm, _ := newTestGateway(t, h)
			gatewaytest.Log = nil
			code, p := getJSON(t, ts.URL+"/_localbase/models/"+gatewaytest.ChatModel+"/admission")
			if code != 200 || p["admitted"] != false {
				t.Fatalf("preview: %d %v", code, p)
			}
			if id, _, _ := llm.Running(); id != "" {
				t.Errorf("the preview started %s", id)
			}
			real := post(t, ts, "/v1/chat/completions", gatewaytest.ChatBody("hi"))
			ref := p["refusal"].(map[string]any)
			var e struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			json.Unmarshal([]byte(real.body), &e)
			if ref["status"].(float64) != float64(real.status) || ref["code"] != real.code() || ref["message"] != e.Error.Message {
				t.Errorf("preview %v\nreal %d %s", ref, real.status, real.body)
			}
			if (ref["retryAfter"] == nil) != (real.header.Get("Retry-After") == "") {
				t.Errorf("Retry-After: %v vs %q", ref["retryAfter"], real.header.Get("Retry-After"))
			}
			if want := real.status == 422; ref["permanent"] != want {
				t.Errorf("permanent: %v at %d", ref["permanent"], real.status)
			}
			logged := false
			for _, line := range strings.Split(logs.String(), "\n") {
				if !strings.Contains(line, "requested_bytes") {
					continue
				}
				var ev struct {
					Attributes struct {
						Requested float64 `json:"requested_bytes"`
					} `json:"attributes"`
				}
				if json.Unmarshal([]byte(line), &ev) == nil && ev.Attributes.Requested > 0 {
					logged = true
					if ev.Attributes.Requested != p["requestedBytes"].(float64) {
						t.Errorf("requested: real %v, preview %v", ev.Attributes.Requested, p["requestedBytes"])
					}
				}
			}
			if name != "GPU can not fit" && !logged {
				t.Errorf("no real requested figure to compare:\n%s", logs.String())
			}
		})
	}
}

func TestAdmissionPreviewOutcomes(t *testing.T) {
	ts, llm, _ := newTestGateway(t, host{})
	base := ts.URL + "/_localbase/models/"
	code, p := getJSON(t, base+gatewaytest.ChatModel+"/admission")
	if code != 200 || p["admitted"] != true || p["refusal"] != nil || p["requestedBytes"].(float64) <= 0 || p["availableBytes"].(float64) <= 0 || p["reserveBytes"].(float64) <= 0 || p["runtime"] != "llm" {
		t.Errorf("fits: %d %v", code, p)
	}
	if id, _, _ := llm.Running(); id != "" {
		t.Errorf("the preview started %s", id)
	}
	if r := post(t, ts, "/v1/chat/completions", gatewaytest.ChatBody("hi")); r.status != 200 {
		t.Fatalf("chat: %d", r.status)
	}
	if _, p := getJSON(t, base+gatewaytest.ChatModel+"/admission"); p["admitted"] != true {
		t.Errorf("already loaded: %v", p)
	}
	if code, _ := getJSON(t, base+"no-such-model/admission"); code != 404 {
		t.Errorf("unknown: %d", code)
	}
	// In the catalog but not on disk, as a real request would be told.
	const absent = "qwen3.5-4b-q4_k_m"
	code, p = getJSON(t, base+absent+"/admission")
	ref, _ := p["refusal"].(map[string]any)
	if code != 200 || p["admitted"] != false || ref == nil || ref["code"] != "model_install_consent_required" {
		t.Errorf("not installed: %d %v", code, p)
	}
}

func TestAdmissionPreviewMedia(t *testing.T) {
	ts, svc := mediaGateway(t, "tts", speechModel, "tts-server", "tts-server")
	code, p := getJSON(t, ts.URL+"/_localbase/models/"+speechModel+"/admission")
	real := post(t, ts, "/v1/audio/speech", speechBody("hello", ""))
	ref, _ := p["refusal"].(map[string]any)
	if code != 200 || p["runtime"] != "tts" || ref == nil || ref["code"] != real.code() || ref["status"].(float64) != float64(real.status) {
		t.Errorf("preview %d %v, real %d %s", code, p, real.status, real.body)
	}
	if svc.Running() != "" {
		t.Errorf("%s started", svc.Running())
	}
}

func TestComputerName(t *testing.T) {
	ts, _, _ := newTestGateway(t, host{})
	name := func() (any, any) {
		_, m := getJSON(t, ts.URL+"/_localbase/host")
		h := m["host"].(map[string]any)
		return h["name"], h["nameSource"]
	}
	put := func(body string) (int, map[string]any) {
		req, _ := http.NewRequest("PUT", ts.URL+"/_localbase/computer-name", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	if n, src := name(); src != "system" || strings.HasSuffix(n.(string), ".local") {
		t.Errorf("default: %v %v", n, src)
	}
	if code, m := put(`{"name":"  Kitchen Mac  "}`); code != 200 || m["name"] != "Kitchen Mac" || m["nameSource"] != "setting" {
		t.Errorf("set: %d %v", code, m)
	}
	if n, src := name(); n != "Kitchen Mac" || src != "setting" {
		t.Errorf("after set: %v %v", n, src)
	}
	for _, bad := range []string{`{}`, `{"name":"` + strings.Repeat("x", 65) + `"}`, `{"name":"a\u0007b"}`, `not json`} {
		if code, _ := put(bad); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code, m := put(`{"name":""}`); code != 200 || m["nameSource"] != "system" {
		t.Errorf("reset: %d %v", code, m)
	}
}

func TestHostRoutesNeedPermission(t *testing.T) {
	ts, _, _ := newConfiguredGateway(t, host{}, func(c *Config) { c.NoAuth = false })
	s := runServer(ts)
	if code, _ := getJSON(t, ts.URL+"/_localbase/host"); code != 401 {
		t.Errorf("host with no key: %d", code)
	}
	if code, _ := getJSON(t, ts.URL+"/_localbase/models/"+gatewaytest.ChatModel+"/admission"); code != 401 {
		t.Errorf("admission with no key: %d", code)
	}
	_, reader, _ := s.cfg.Store.CreateAPIKey(context.Background(), "reader", []string{"models:read"})
	_, chat, _ := s.cfg.Store.CreateAPIKey(context.Background(), "chat", []string{"inference:chat"})
	send := func(method, path, key, body string) int {
		req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := send("GET", "/_localbase/host", reader, ""); code != 200 {
		t.Errorf("models:read: %d", code)
	}
	if code := send("GET", "/_localbase/host", chat, ""); code != 403 {
		t.Errorf("without models:read: %d", code)
	}
	if code := send("PUT", "/_localbase/computer-name", reader, `{"name":"x"}`); code != 403 {
		t.Errorf("rename without models:manage: %d", code)
	}
}

func TestQuietRoutes(t *testing.T) {
	for _, c := range []struct {
		label, path string
		want        bool
	}{{"/health", "/health", true}, {"/health/ready", "/health/ready", true}, {"/app/", uiPollPath, true}, {"/app/", "/app/chat", false}, {"/v1/models", "/v1/models", false}} {
		if quietRoute(c.label, c.path) != c.want {
			t.Errorf("%v", c)
		}
	}
}

// With assumeInstalled, a model not on disk gets the memory answer instead of the
// install refusal: admitted, or the same refusal an installed model would get.
func TestAdmissionPreviewAssumeInstalled(t *testing.T) {
	const absent = "qwen3.5-4b-q4_k_m"
	path := "/_localbase/models/" + absent + "/admission"
	ts, _, _ := newTestGateway(t, host{})
	code, p := getJSON(t, ts.URL+path+"?assumeInstalled=1")
	if code != 200 || p["admitted"] != true || p["refusal"] != nil || p["requestedBytes"].(float64) <= 0 || p["reserveBytes"].(float64) <= 0 || p["availableBytes"].(float64) <= 0 || p["demandSource"] == "" {
		t.Errorf("fits: %d %v", code, p)
	}
	if _, plain := getJSON(t, ts.URL+path); plain["admitted"] != false || plain["refusal"].(map[string]any)["code"] != "model_install_consent_required" {
		t.Errorf("without the parameter: %v", plain)
	}
	ts, _, _ = newTestGateway(t, fixedHost{8 * memory.GiB, 7 * memory.GiB})
	_, p = getJSON(t, ts.URL+path+"?assumeInstalled=1")
	ref, _ := p["refusal"].(map[string]any)
	if p["admitted"] != false || ref == nil || ref["code"] != "model_too_large" || ref["permanent"] != true || ref["status"] != 422.0 {
		t.Errorf("can never fit: %v", p)
	}
	ts, _, _ = newTestGateway(t, fixedHost{64 * memory.GiB, 10 * memory.GiB})
	_, p = getJSON(t, ts.URL+path+"?assumeInstalled=1")
	ref, _ = p["refusal"].(map[string]any)
	if p["admitted"] != false || ref == nil || ref["code"] != "insufficient_memory" || ref["permanent"] != false {
		t.Errorf("low right now: %v", p)
	}
	// The same figure the model would be admitted with once installed: the estimate
	// does not depend on the files being present.
	ts, _, _ = newTestGatewayWith(t, host{}, absent)
	_, installed := getJSON(t, ts.URL+"/_localbase/models/"+absent+"/admission")
	ts, _, _ = newTestGateway(t, host{})
	_, assumed := getJSON(t, ts.URL+path+"?assumeInstalled=1")
	t.Logf("installed %v (%v), assumed %v (%v)", installed["requestedBytes"], installed["demandSource"], assumed["requestedBytes"], assumed["demandSource"])
	if installed["requestedBytes"] != assumed["requestedBytes"] {
		t.Errorf("estimate %v, installed %v", assumed["requestedBytes"], installed["requestedBytes"])
	}
}

// settling is there only while refusals for pressure can still happen after memory
// has recovered, and the reading never says it otherwise.
func TestHostSettling(t *testing.T) {
	h := &steppedHost{}
	h.avail.Store(60 * memory.GiB)
	ts, _, mem := newTestGateway(t, h)
	settling := func() (bool, bool) {
		t.Helper()
		_, m := getJSON(t, ts.URL+"/_localbase/host")
		v, present := hostMem(m)["settling"]
		return v == true, present
	}
	if _, present := settling(); present {
		t.Error("settling on a healthy host")
	}
	h.avail.Store(1 * memory.GiB)
	mem.Poll(context.Background())
	if _, present := settling(); present {
		t.Error("settling while memory is still low")
	}
	h.avail.Store(60 * memory.GiB)
	mem.Poll(context.Background())
	if on, _ := settling(); !on {
		t.Error("not settling after memory recovered")
	}
	mem.Poll(context.Background())
	mem.Poll(context.Background())
	if _, present := settling(); present {
		t.Error("settling after the state recovered")
	}
}
