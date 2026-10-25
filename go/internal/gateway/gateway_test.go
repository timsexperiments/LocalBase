package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

var fakeBinary string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "lbgo-gw-fake-")
	fakeBinary = filepath.Join(dir, "llama-server")
	if out, err := exec.Command("go", "build", "-o", fakeBinary, "../runtime/fakebackend").CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, string(out))
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func freePort(t testing.TB) int {
	for i := 0; i < 200; i++ {
		p := 24000 + rand.IntN(6000)
		if p == 24173 || (p >= 25611 && p <= 25613) {
			continue
		}
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			ln.Close()
			return p
		}
	}
	t.Fatal("no free port")
	return 0
}

type host struct{}

func (host) Topology() memory.Topology {
	return memory.Topology{Unified: true, SystemCapacity: 64 * memory.GiB, GPUWorkingSetBytes: 48 * memory.GiB}
}
func (host) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: 50 * memory.GiB, Pressure: "normal"}
}

const (
	chatModel      = "qwen3.5-9b-q4_k_m"
	embeddingModel = "qwen3-embedding-0.6b-q8_0"
	decisionModel  = "clef-flash-q4_k_m"
)

// newTestServer runs a gateway with --no-auth over the fake backend.
// testLog, when set, receives the next test gateway's JSON log.
var testLog io.Writer

// testPlacement, when set, is the next test gateway's placement source.
var testPlacement *runtime.HostPlacement

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts, _, _ := newTestGateway(t, host{})
	return ts
}

// newTestGateway is newTestServer on a given host, with its LLM runtime and
// memory controller.
func newTestGateway(t *testing.T, h memory.HostProvider) (*httptest.Server, *runtime.LLM, *memory.Controller) {
	t.Helper()
	return newTestGatewayWith(t, h)
}

// newTestGatewayWith also installs and selects extra LLMs.
func newTestGatewayWith(t *testing.T, h memory.HostProvider, extra ...string) (*httptest.Server, *runtime.LLM, *memory.Controller) {
	t.Helper()
	dir := t.TempDir()
	r, err := root.Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	models := r.ModelsDir("llm")
	os.MkdirAll(models, 0o700)
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range append([]string{chatModel, embeddingModel, decisionModel}, extra...) {
		installSparse(t, models, cat.ByID(id))
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &store.Config{Host: "127.0.0.1", Port: freePort(t), CtxSize: 8192, CtxMode: "auto", Parallel: "1",
		Active:   map[string]string{"llm": chatModel},
		Selected: map[string][]string{"llm": append([]string{chatModel, embeddingModel, decisionModel}, extra...)}}
	cfg.Root, cfg.GatewayHost, cfg.SttHost = r.Dir, "127.0.0.1", "127.0.0.1"
	if err := st.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	mem := memory.NewController(h, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testLog != nil {
		log = slog.New(slog.NewJSONHandler(testLog, nil))
	}
	llm := runtime.NewLLM(runtime.LLMDeps{Root: r.Dir, ModelsDir: models, Store: st, Catalog: cat, Memory: mem,
		Log: log, MemoryGB: 64, Binary: fakeBinary, QueueCapacity: 4, QueueTimeout: 5 * time.Second}, cfg)
	var placementSrc *runtime.HostPlacement
	if testPlacement != nil {
		placementSrc = testPlacement
		placementSrc.Store, placementSrc.Memory, placementSrc.Log = st, mem, log
		llm.SetPlacementSource(placementSrc)
	}
	t.Cleanup(llm.Close)
	srv := New(&Server{Store: st, Catalog: cat, Auth: &auth.Resolver{Store: st, Mode: "either"}, LLM: llm, Log: log, Placement: placementSrc,
		NoAuth: true, MemoryGB: 64, Root: r, Memory: mem})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, llm, mem
}

type result struct {
	status int
	header http.Header
	body   string
}

func post(t *testing.T, ts *httptest.Server, path, body string) result {
	t.Helper()
	resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header, string(b)}
}

func (r result) code() string {
	var e struct{ Error struct{ Code any } }
	json.Unmarshal([]byte(r.body), &e)
	return fmt.Sprint(e.Error.Code)
}

func TestChatValidation(t *testing.T) {
	ts := newTestServer(t)
	cases := []struct{ body, wantCode string }{
		{`{"messages":[]}`, "validation_failed"},
		{`{"model":" ","messages":[]}`, "validation_failed"},
		{`{"model":"m","messages":[{"role":"robot","content":"x"}]}`, "validation_failed"},
		{`{"model":"m","messages":[{"role":"assistant"}]}`, "validation_failed"},
		{`{"model":"m","messages":[],"temperature":3}`, "validation_failed"},
		{`{"model":"m","messages":[],"tool_choice":{"type":"function","function":{"name":"x"}}}`, "validation_failed"},
		{`{"model":"m","messages":[],"response_format":{"type":"json_schema","json_schema":{"name":"bad name","schema":{}}}}`, "validation_failed"},
		{`not json`, "validation_failed"},
		{`{"model":"nope","messages":[{"role":"user","content":"x"}]}`, "model_not_found"},
	}
	for _, c := range cases {
		got := post(t, ts, "/v1/chat/completions", c.body)
		if got.code() != c.wantCode {
			t.Errorf("%s: %d %s", c.body, got.status, got.body)
		}
	}
	got := post(t, ts, "/v1/chat/completions", `{"model":"m","messages":[],"response_format":{"type":"json_schema","json_schema":{"name":"bad name","schema":{}}}}`)
	if !strings.Contains(got.body, `"param":"response_format"`) {
		t.Errorf("response_format param missing: %s", got.body)
	}
}

func TestChatModelKindRejections(t *testing.T) {
	ts := newTestServer(t)
	got := post(t, ts, "/v1/chat/completions", `{"model":"`+decisionModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if got.status != 400 || !strings.Contains(got.body, "decision model") {
		t.Errorf("decision: %d %s", got.status, got.body)
	}
	got = post(t, ts, "/v1/chat/completions", `{"model":"`+embeddingModel+`","messages":[{"role":"user","content":"hi"}]}`)
	if got.status != 400 || !strings.Contains(got.body, "embeddings only") {
		t.Errorf("embedding: %d %s", got.status, got.body)
	}
	got = post(t, ts, "/v1/systemone", `{"model":"`+chatModel+`","state":null,"questions":{"q":{"type":"noul"}}}`)
	if got.status != 400 || got.code() != "unsupported_model_capability" {
		t.Errorf("systemone on chat model: %d %s", got.status, got.body)
	}
	got = post(t, ts, "/v1/embeddings", `{"model":"`+decisionModel+`","input":"x"}`)
	if got.status != 400 || !strings.Contains(got.body, "decision model") {
		t.Errorf("embeddings on decision model: %d %s", got.status, got.body)
	}
}

func TestStructuredOutputRejections(t *testing.T) {
	ts := newTestServer(t)
	wrap := func(schema string) string {
		return `{"model":"` + chatModel + `","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"s","schema":` + schema + `}}}`
	}
	cases := []struct{ schema, code string }{
		{`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"],"additionalProperties":false}`, ""},
		{`{"type":"object","properties":{"a":{"type":"string","pattern":"x"}},"required":["a"],"additionalProperties":false}`, "unsupported_json_schema"},
		{`{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`, "unsupported_json_schema"},
		{`{"type":"object","foo":1}`, "invalid_json_schema"},
		{`{"type":"strng"}`, "invalid_json_schema"},
		{`{"type":"object","properties":{},"required":["missing"],"additionalProperties":false}`, "invalid_json_schema"},
		{`{"type":"array","items":{"type":"string"}}`, "unsupported_json_schema"},
		{`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`, "unsupported_json_schema"},
	}
	for _, c := range cases {
		got := post(t, ts, "/v1/chat/completions", wrap(c.schema))
		if c.code == "" {
			if got.status == 400 {
				t.Errorf("%s rejected: %s", c.schema, got.body)
			}
			continue
		}
		if got.status != 400 || got.code() != c.code {
			t.Errorf("%s: want %s, got %d %s", c.schema, c.code, got.status, got.body)
		}
	}
}

func TestStructuredOutputValidatesCompletedResponse(t *testing.T) {
	ts := newTestServer(t)
	schema := `{"type":"object","properties":{"a":{"type":"integer","minimum":1}},"required":["a"],"additionalProperties":false}`
	req := func(reply string) string {
		return `{"model":"` + chatModel + `","messages":[{"role":"user","content":"fake:reply ` + reply + `"}],"response_format":{"type":"json_schema","json_schema":{"name":"s","schema":` + schema + `}}}`
	}
	if got := post(t, ts, "/v1/chat/completions", req(`{\"a\":2}`)); got.status != 200 {
		t.Errorf("valid reply: %d %s", got.status, got.body)
	} else if !strings.Contains(got.header.Get("Server-Timing"), "prepare;dur=") {
		t.Errorf("Server-Timing: %q", got.header.Get("Server-Timing"))
	}
	if got := post(t, ts, "/v1/chat/completions", req(`{\"a\":0}`)); got.status != 502 || got.code() != "structured_output_validation_failed" {
		t.Errorf("invalid reply: %d %s", got.status, got.body)
	}
}

func TestChatResponseNormalization(t *testing.T) {
	ts := newTestServer(t)
	got := post(t, ts, "/v1/chat/completions", `{"model":"`+chatModel+`","messages":[{"role":"user","content":"fake:reasoning"}]}`)
	if got.status != 200 || !strings.Contains(got.body, `"reasoning":"hm"`) || strings.Contains(got.body, "reasoning_content") {
		t.Errorf("reasoning: %s", got.body)
	}
	if !strings.Contains(got.body, `"model":"`+chatModel+`"`) {
		t.Errorf("public model id: %s", got.body)
	}
	if !strings.Contains(got.body, "max_tokens=4096") {
		t.Errorf("default max_tokens: %s", got.body)
	}
	stream := post(t, ts, "/v1/chat/completions", `{"model":"`+chatModel+`","stream":true,"messages":[{"role":"user","content":"fake:reasoning"}]}`)
	if !strings.Contains(stream.body, `"reasoning":"hm"`) || strings.Contains(stream.body, "reasoning_content") ||
		!strings.HasSuffix(stream.body, "data: [DONE]\n\n") {
		t.Errorf("stream: %s", stream.body)
	}
	trunc := post(t, ts, "/v1/chat/completions", `{"model":"`+chatModel+`","stream":true,"messages":[{"role":"user","content":"fake:truncated"}]}`)
	if !strings.Contains(trunc.body, "upstream_error") {
		t.Errorf("truncated stream: %s", trunc.body)
	}
}

func TestContextOverflowIsOpenAIShaped(t *testing.T) {
	ts := newTestServer(t)
	got := post(t, ts, "/v1/chat/completions", `{"model":"`+chatModel+`","messages":[{"role":"user","content":"fake:context"}]}`)
	if got.status != 400 || got.code() != "context_length_exceeded" || !strings.Contains(got.body, `"param":"messages"`) {
		t.Errorf("%d %s", got.status, got.body)
	}
}

func TestToolGate(t *testing.T) {
	ts := newTestServer(t)
	body := `{"model":"` + chatModel + `","messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"s":{"type":"string","pattern":"a|b"}}}}}],"tool_choice":{"type":"function","function":{"name":"f"}}}`
	got := post(t, ts, "/v1/chat/completions", body)
	if got.status != 200 {
		t.Errorf("declared tool-calling model: %d %s", got.status, got.body)
	}
	// The fake reports no tool support once the runtime probe recorded it.
	time.Sleep(300 * time.Millisecond)
	got = post(t, ts, "/v1/chat/completions", body)
	if got.status != 400 || got.code() != "unsupported_model_capability" || !strings.Contains(got.body, `"param":"tools"`) {
		t.Errorf("after probe: %d %s", got.status, got.body)
	}
}

func TestAnchorPattern(t *testing.T) {
	for in, want := range map[string]string{"a": "^a$", "^a$": "^a$", "a|b": "^(?:a|b)$", `a\$`: `^a\$$`, "^a": "^a$"} {
		if got := anchorPattern(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}

func TestEmbeddingsAndSystemOne(t *testing.T) {
	ts := newTestServer(t)
	got := post(t, ts, "/v1/embeddings", `{"model":"`+embeddingModel+`","input":["a","b"]}`)
	if got.status != 200 || !strings.Contains(got.body, `"model":"`+embeddingModel+`"`) {
		t.Errorf("embeddings: %d %s", got.status, got.body)
	}
	if got := post(t, ts, "/v1/embeddings", `{"model":"`+embeddingModel+`","input":[1,"a"]}`); got.code() != "validation_failed" {
		t.Errorf("mixed input: %s", got.body)
	}
	got = post(t, ts, "/v1/systemone", `{"model":"`+decisionModel+`","state":"x","questions":{"done":{"type":"noul"}}}`)
	if got.status != 200 || !strings.Contains(got.body, `"noul"`) {
		t.Errorf("systemone: %d %s", got.status, got.body)
	}
	for _, body := range []string{
		`{"model":"` + decisionModel + `","state":1,"questions":{}}`,
		`{"model":"` + decisionModel + `","state":null,"questions":{"q":{"type":"choice","criteria":{"a":null}}}}`,
		`{"model":"` + decisionModel + `","state":null,"questions":{"q":{"type":"score","criteria":["a"]}}}`,
		`{"model":"` + decisionModel + `","state":null,"files":["x"],"questions":{"q":{"type":"noul"}}}`,
		`{"model":"` + decisionModel + `","state":null,"extra":1,"questions":{"q":{"type":"noul"}}}`,
	} {
		if got := post(t, ts, "/v1/systemone", body); got.code() != "validation_failed" {
			t.Errorf("%s: %d %s", body, got.status, got.body)
		}
	}
}

// installSparse creates a model's artifacts at their expected sizes as
// sparse files, so the install check passes without real weights.
func installSparse(t *testing.T, dir string, s *catalog.Spec) {
	t.Helper()
	for _, a := range s.Artifacts {
		f, err := os.Create(filepath.Join(dir, a.Filename))
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString("not a real gguf")
		if err := f.Truncate(max(a.ExpectedSizeBytes, 15)); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}
