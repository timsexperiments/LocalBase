package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

type tsGeometry struct {
	Architecture          string `json:"architecture"`
	BlockCount            int    `json:"blockCount"`
	FullKvHeads           int    `json:"fullKvHeads"`
	SwaKvHeads            int    `json:"swaKvHeads"`
	SlidingWindow         *int   `json:"slidingWindow"`
	KeyLength             int    `json:"keyLength"`
	ValueLength           int    `json:"valueLength"`
	SwaKeyLength          int    `json:"swaKeyLength"`
	SwaValueLength        int    `json:"swaValueLength"`
	Q8Compatible          bool   `json:"q8Compatible"`
	RecurrentBytesPerSlot int64  `json:"recurrentBytesPerSlot"`
	ContextLength         *int   `json:"contextLength"`
	DecisionType          string `json:"decisionType"`
}

func (g *tsGeometry) toGo() *memory.Geometry {
	if g == nil {
		return nil
	}
	out := &memory.Geometry{Architecture: g.Architecture, BlockCount: g.BlockCount, FullKVHeads: g.FullKvHeads,
		SWAKVHeads: g.SwaKvHeads, KeyLength: g.KeyLength, ValueLength: g.ValueLength, SWAKeyLength: g.SwaKeyLength,
		SWAValueLength: g.SwaValueLength, Q8Compatible: g.Q8Compatible, RecurrentBytesPerSlot: g.RecurrentBytesPerSlot,
		DecisionType: g.DecisionType}
	if g.SlidingWindow != nil {
		out.SlidingWindow = *g.SlidingWindow
	}
	if g.ContextLength != nil {
		out.ContextLength = *g.ContextLength
	}
	return out
}

type goldenCase struct {
	ModelID string `json:"modelId"`
	Config  struct {
		CtxSize  int    `json:"ctxSize"`
		CtxMode  string `json:"ctxMode"`
		Parallel any    `json:"parallel"`
	} `json:"config"`
	MemoryGB float64     `json:"memoryGb"`
	Geometry *tsGeometry `json:"geometry"`
	Error    string      `json:"error"`
	CtxSize  int         `json:"ctxSize"`
	Slots    int         `json:"slots"`
	KVType   string      `json:"kvType"`
	Demand   struct {
		UnifiedBytes     int64  `json:"unifiedBytes"`
		HostBytes        int64  `json:"hostBytes"`
		AcceleratorBytes int64  `json:"acceleratorBytes"`
		Confidence       string `json:"confidence"`
	} `json:"demand"`
	Args   []string      `json:"args"`
	Budget ContextBudget `json:"budget"`
}

func loadGolden(t *testing.T) (string, []goldenCase) {
	raw, err := os.ReadFile("testdata/llm-plans.json")
	if err != nil {
		t.Fatal(err)
	}
	var g struct {
		Platform string       `json:"platform"`
		Cases    []goldenCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g.Platform, g.Cases
}

// TestPlanMatchesTypeScript compares every launch plan, demand, argv and
// context budget with the TS implementation's output.
func TestPlanMatchesTypeScript(t *testing.T) {
	platform, cases := loadGolden(t)
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		name := fmt.Sprintf("%s/%d-%s-%v/%vGB", c.ModelID, c.Config.CtxSize, c.Config.CtxMode, c.Config.Parallel, c.MemoryGB)
		t.Run(name, func(t *testing.T) {
			cfg := &store.Config{Host: "127.0.0.1", CtxSize: c.Config.CtxSize, CtxMode: c.Config.CtxMode,
				Parallel: fmt.Sprint(c.Config.Parallel)}
			p, err := PlanLLM(PlanInput{Config: cfg, Spec: cat.ByID(c.ModelID), ModelID: c.ModelID,
				ModelsDir: "/m", MemoryGB: c.MemoryGB, Port: 18000, Geometry: c.Geometry.toGo()})
			if c.Error != "" {
				if err == nil {
					t.Fatalf("want error %q", c.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.CtxSize != c.CtxSize || p.Parallel.Slots != c.Slots || p.KVType != c.KVType {
				t.Errorf("ctx/slots/kv = %d/%d/%s, want %d/%d/%s", p.CtxSize, p.Parallel.Slots, p.KVType, c.CtxSize, c.Slots, c.KVType)
			}
			d := p.Demand
			if d.UnifiedBytes != c.Demand.UnifiedBytes || d.HostBytes != c.Demand.HostBytes ||
				d.AcceleratorBytes != c.Demand.AcceleratorBytes || d.Confidence != c.Demand.Confidence {
				t.Errorf("demand = %+v, want %+v", d, c.Demand)
			}
			if got := p.Args(platform == "darwin-arm64"); !reflect.DeepEqual(got, c.Args) {
				t.Errorf("args\n got %s\nwant %s", strings.Join(got, " "), strings.Join(c.Args, " "))
			}
			if !reflect.DeepEqual(p.Budget, c.Budget) {
				gb, _ := json.Marshal(p.Budget)
				wb, _ := json.Marshal(c.Budget)
				t.Errorf("budget\n got %s\nwant %s", gb, wb)
			}
		})
	}
}

// TestGeometryMatchesTypeScript reads the same GGUF headers the golden
// generator read (LOCALBASE_TEST_MODELS_DIR, read only).
func TestGeometryMatchesTypeScript(t *testing.T) {
	dir := os.Getenv("LOCALBASE_TEST_MODELS_DIR")
	if dir == "" {
		t.Skip("set LOCALBASE_TEST_MODELS_DIR to the models/llm directory the golden file was built from")
	}
	_, cases := loadGolden(t)
	cat, _ := catalog.Load("")
	seen := map[string]bool{}
	for _, c := range cases {
		if seen[c.ModelID] || c.Geometry == nil {
			continue
		}
		seen[c.ModelID] = true
		path := filepath.Join(dir, cat.ByID(c.ModelID).Primary().Filename)
		got := memory.ReadGeometry(path)
		if !reflect.DeepEqual(got, c.Geometry.toGo()) {
			t.Errorf("%s geometry\n got %+v\nwant %+v", c.ModelID, got, c.Geometry.toGo())
		}
	}
	if len(seen) == 0 {
		t.Skip("no golden case had geometry")
	}
}
