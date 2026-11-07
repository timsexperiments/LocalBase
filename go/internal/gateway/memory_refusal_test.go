package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

func refusal(reason string, reserved int64) *memory.AdmissionError {
	return &memory.AdmissionError{Reason: reason, PoolID: "system", RequestedBytes: 8 * memory.GiB, ReservedBytes: reserved,
		Diagnostics: &memory.AdmissionDiagnostics{Requested: 8 * memory.GiB, Reserve: 96 * memory.GiB / 10, EffectiveAvailable: 128 * memory.GiB / 10}}
}

// The text the operator approved, word for word.
func TestMemoryRefusalWords(t *testing.T) {
	const held = 12 * memory.GiB
	for _, c := range []struct {
		name string
		e    *memory.AdmissionError
		want string
	}{
		{"not enough free", refusal("system-memory", held), "Not enough free memory to start this model: it needs 8.0 GiB, and 3.2 GiB is free after keeping 9.6 GiB for your computer. LocalBase's loaded models have 12.0 GiB reserved. Free memory from a loaded model or close other apps, then try again."},
		{"not enough free, nothing loaded", refusal("system-memory", 0), "Not enough free memory to start this model: it needs 8.0 GiB, and 3.2 GiB is free after keeping 9.6 GiB for your computer. LocalBase has nothing loaded, so other apps are using this memory. Close other apps, then try again."},
		{"pressure", refusal("memory-pressure", held), "This computer is under memory pressure, so this model wasn't started. It needs 8.0 GiB, and LocalBase's loaded models have 12.0 GiB reserved. Free memory from a loaded model or close other apps, then try again."},
		{"pressure, nothing loaded", refusal("memory-pressure", 0), "This computer is under memory pressure, so this model wasn't started. It needs 8.0 GiB. LocalBase has nothing loaded, so other apps are using this memory. Close other apps, then try again."},
		{"anything else keeps the general sentence", refusal("measurement-unavailable", held), "Insufficient available memory to start the requested runtime. Please try again shortly."},
	} {
		if got := httpx.MemoryRefusal(c.e, "this model"); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

// Over HTTP, only the words changed.
func TestAMemoryRefusalKeepsItsCodeStatusAndRetryAfter(t *testing.T) {
	for _, e := range []*memory.AdmissionError{refusal("system-memory", memory.GiB), refusal("memory-pressure", 0), refusal("measurement-unavailable", 0)} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		httpx.RuntimeError(rec, req, e, "m", "Inference")
		if rec.Code != 503 || rec.Header().Get("Retry-After") != "5" {
			t.Errorf("%s: %d, Retry-After %q", e.Reason, rec.Code, rec.Header().Get("Retry-After"))
		}
		body := rec.Body.String()
		if want := jsonEscaped(httpx.MemoryRefusal(e, "this model")); !strings.Contains(body, want) {
			t.Errorf("%s: the API doesn't send the words: %s", e.Reason, body)
		}
		if !strings.Contains(body, `"code":"insufficient_memory"`) || !strings.Contains(body, `"type":"api_error"`) {
			t.Errorf("%s: %s", e.Reason, body)
		}
	}
}

// What a loaded model has reserved comes from the controller, and leaves out one being replaced.
func TestARefusalCountsWhatIsReserved(t *testing.T) {
	h := &steppedHost{}
	h.avail.Store(50 * memory.GiB)
	mem := memory.NewController(h, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	res, err := mem.Reserve(t.Context(), "llm", memory.Demand{UnifiedBytes: 12 * memory.GiB, HostBytes: 12 * memory.GiB, AcceleratorBytes: memory.GiB})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Release()
	h.avail.Store(9 * memory.GiB)
	big := memory.Demand{UnifiedBytes: 20 * memory.GiB, HostBytes: 20 * memory.GiB, AcceleratorBytes: memory.GiB}
	if p := mem.Preview(t.Context(), big, nil); p.Err == nil {
		t.Fatal("should have been refused")
	}
	err = mem.CheckAdmission(t.Context(), big, nil)
	ae, ok := err.(*memory.AdmissionError)
	if !ok || ae.ReservedBytes != 12*memory.GiB {
		t.Errorf("%v (%T)", err, err)
	}
	err = mem.CheckAdmission(t.Context(), big, []string{"llm"})
	if ae, ok := err.(*memory.AdmissionError); ok && ae.ReservedBytes != 0 {
		t.Errorf("one being replaced still counted: %d", ae.ReservedBytes)
	}
}

// A failed install says it can go on only when there is a part to go on from.
func TestAFailedInstallMentionsAPartOnlyWhenOneWasKept(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	var spec *catalog.Spec
	for _, c := range s.Catalog.All() {
		if len(c.Artifacts) > 0 {
			spec = c
			break
		}
	}
	const failed = "Installation failed. Check storage, network access, and catalog verification."
	if got := s.failedInstallDetail(spec); got != failed {
		t.Errorf("with no part: %q", got)
	}
	dir := s.modelsDir(spec.Kind)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(dir, spec.Artifacts[0].Filename) + ".partial"
	if err := os.WriteFile(part, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.failedInstallDetail(spec); got != failed {
		t.Errorf("with an empty part, nothing to go on from: %q", got)
	}
	if err := os.WriteFile(part, []byte("some"), 0o600); err != nil {
		t.Fatal(err)
	}
	const want = failed + " The downloaded part is kept, so installing again continues from it (or starts over if the file has changed)."
	if got := s.failedInstallDetail(spec); got != want {
		t.Errorf("with a part: %q", got)
	}
}

func jsonEscaped(text string) string {
	b, _ := json.Marshal(text)
	return string(b)
}

// The Running page's button is "Free memory", so telling someone to unload names a control they won't
// find. "Unload" stays where it describes the API route or a state, which are listed. This reads the
// source, so a new message is covered without being named here.
func TestRefusalsSayFreeMemoryNotUnload(t *testing.T) {
	allowed := []string{
		"This model is being unloaded.", // model_unloading: a state
		"or unload with waiting.",       // about the API call that was just made
		"Model unload",                  // the name of that service
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	sentence := regexp.MustCompile(`"[^"\n]*\b[Uu]nload[^"\n]*[ .][^"\n]*"`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checked++
	next:
		for _, m := range sentence.FindAllString(string(b), -1) {
			for _, ok := range allowed {
				if strings.Contains(m, ok) {
					continue next
				}
			}
			t.Errorf("%s: a sentence with unload that isn't on the allowed list: %s", f, m)
		}
	}
	if checked < 10 {
		t.Fatalf("read only %d source files", checked)
	}
	// What the tests above pin must be what people read.
	for _, text := range []string{freeMemoryHint, httpx.MemoryRefusal(refusal("system-memory", memory.GiB), "this model")} {
		if !strings.Contains(text, "Free memory from a loaded model") {
			t.Errorf("%q", text)
		}
	}
	working := videoMemoryMessage(&memory.AdmissionError{Reason: "accelerator-memory", PoolID: "gpu-working-set"})
	if !strings.Contains(working, "Free memory from a loaded model, then try again.") {
		t.Errorf("%q", working)
	}
	rec := httptest.NewRecorder()
	httpx.RuntimeError(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), &memory.AdmissionError{Reason: "accelerator-memory", PoolID: "gpu-working-set"}, "m", "Inference")
	if !strings.Contains(rec.Body.String(), "Retry once another model is idle, or free memory from one.") {
		t.Errorf("%s", rec.Body.String())
	}
}

// The differential tests no longer compare a memory refusal's words with TS, so they have to be pinned
// here. This fails if the golden stops holding them, or the words stop being the golden's.
func TestTheRefusalWordsArePinnedInTheGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/video.json")
	if err != nil {
		t.Fatal(err)
	}
	const nothing = "LocalBase has nothing loaded, so other apps are using this memory. Close other apps, then try again."
	for _, want := range []string{
		"This computer is under memory pressure, so video generation wasn't started. " + nothing,
		"Not enough free memory to start video generation: it needs 12.0 GiB, and 5.4 GiB is free after keeping 9.6 GiB for your computer. " + nothing,
	} {
		if !strings.Contains(string(raw), jsonEscaped(want)) {
			t.Errorf("not in the golden: %s", want)
		}
	}
	e := refusal("memory-pressure", 0)
	e.Diagnostics = nil
	e.RequestedBytes = 0
	if got := httpx.MemoryRefusal(e, "video generation"); !strings.Contains(string(raw), jsonEscaped(got)) {
		t.Errorf("the code's words aren't the golden's: %s", got)
	}
}
