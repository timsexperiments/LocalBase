package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

const placementState = `{"model":"qwen3.5-4b-q4_k_m",
 "requested":{"mode":"custom","gpuLayers":0,"cpuMoeLayers":null,"contextSize":16384,"contextMode":"explicit","contextScope":"all models"},
 "loaded":true,"pendingNextLoad":true,
 "resolved":{"mode":"auto","platform":"discrete-nvidia","gpuLayers":25,"totalLayers":25,"cpuMoeLayers":4,"hostTensors":24,"hostLayers":[0,1,2,3],
  "hostBytes":1696481280,"freeVramBytes":12476481536,"reserveBytes":536870912,"note":"Experts of layers 0–3 stay in system memory (1.6 GB); everything else is on the GPU.","plannedAt":"2026-10-25T10:00:00Z"},
 "next":{"mode":"custom","platform":"discrete-nvidia","gpuLayers":0,"totalLayers":25,"cpuMoeLayers":0,"hostBytes":12000000000,"note":"Runs on the CPU; uses 11.2 GB of system memory."}}`

func placementUI(t *testing.T, perms []string, calls *[]string) *UI {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cat, _ := catalog.Load("")
	p := &auth.Principal{Kind: "browser-session", ID: "u", Permissions: perms}
	call := func(_ context.Context, _ *auth.Principal, method, path, _ string, body []byte) APIResult {
		*calls = append(*calls, method+" "+path+" "+string(body))
		if method == "GET" {
			return APIResult{Status: 200, Body: []byte(placementState)}
		}
		return APIResult{Status: 200, Body: []byte(placementState)}
	}
	return New(Deps{SignInEnabled: true, Principal: func(*http.Request) *auth.Principal { return p }, Call: call, Catalog: cat, Store: st,
		RootDir: dir, Memory: memory.NewController(memory.NewHostProvider(), memory.Reserve{Percent: 15, MinimumGB: 8}, false)})
}

// The Placement card: the loaded split and the next load's, in capacity
// words, "applies on next load", and the advanced form for managers.
func TestPlacementCard(t *testing.T) {
	var calls []string
	u := placementUI(t, []string{"models:read", "models:manage"}, &calls)
	body := get(u, "/app/models/m/qwen3.5-4b-q4_k_m").Body.String()
	for _, want := range []string{"Placement", "Applies on next load", "Loaded now", "Experts of layers 0–3 stay in system memory",
		"25 of 25 layers on the GPU", "Expert weights of layers 0–3 (24 tensors) in system memory", "1.6 GB of system memory used by the model",
		"Planned at 2026-10-25T10:00:00Z against 11.6 GB of free GPU memory (0.5 GB kept free)", "Next load", "Runs on the CPU",
		`<option value="custom" selected>`, `name="gpu_layers" type="number" min="0" inputmode="numeric" class="input min-h-11" value="0"`,
		"shared by all models", "apply the next time the model loads"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, banned := range []string{"faster", "slower", "tok/s", "speed"} {
		if strings.Contains(strings.ToLower(body[strings.Index(body, "placement-card"):]), banned) {
			t.Errorf("speed wording %q in the card", banned)
		}
	}
	// Readers see the split, not the form.
	var none []string
	reader := get(placementUI(t, []string{"models:read"}, &none), "/app/models/m/qwen3.5-4b-q4_k_m").Body.String()
	if !strings.Contains(reader, "Loaded now") || strings.Contains(reader, `hx-post="/app/models/placement/`) {
		t.Error("a reader got the placement form, or no split")
	}
}

// The form saves through the API: PUT with the custom values and a changed
// context size; Back to auto is DELETE.
func TestPlacementCardSaves(t *testing.T) {
	var calls []string
	u := placementUI(t, []string{"models:read", "models:manage"}, &calls)
	post := func(form url.Values) string {
		req := httptest.NewRequest("POST", "/app/models/placement/qwen3.5-4b-q4_k_m", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("X-Localbase-Ui", "1")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		withCSRF(u, req)
		w := httptest.NewRecorder()
		u.ServeHTTP(w, req)
		return w.Body.String()
	}
	calls = nil
	out := post(url.Values{"action": {"save"}, "mode": {"custom"}, "gpu_layers": {"10"}, "cpu_moe_layers": {""}, "context_size": {"32768"}, "context_size_was": {"16384"}})
	var put string
	for _, c := range calls {
		if strings.HasPrefix(c, "PUT ") {
			put = c
		}
	}
	var sent map[string]any
	json.Unmarshal([]byte(put[strings.Index(put, "{"):]), &sent)
	if sent["mode"] != "custom" || sent["gpuLayers"] != 10.0 || sent["contextSize"] != 32768.0 || sent["cpuMoeLayers"] != nil {
		t.Errorf("PUT %s", put)
	}
	if !strings.Contains(out, "applies the next time the model loads") {
		t.Errorf("no confirmation: %s", out)
	}
	calls = nil
	post(url.Values{"action": {"save"}, "mode": {"auto"}, "context_size": {"16384"}, "context_size_was": {"16384"}})
	if len(calls) == 0 || strings.Contains(strings.Join(calls, "\n"), "contextSize") {
		t.Errorf("an unchanged context size was sent: %v", calls)
	}
	calls = nil
	post(url.Values{"action": {"reset"}})
	if len(calls) == 0 || !strings.HasPrefix(calls[0], "DELETE /_localbase/models/qwen3.5-4b-q4_k_m/placement") {
		t.Errorf("reset: %v", calls)
	}
}
