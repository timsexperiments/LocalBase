package conf

import (
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// defaultLlmModels in the declarative document (Go only, DESIGN §18).
func TestDefaultLLMModelsDocument(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base := Default("/r", 64, cat)
	base.Selected["llm"] = []string{"qwen3.6-35b-a3b-iq4_nl_xl", "qwen3.5-4b-q4_k_m", "clef-flash-q4_k_m"}
	base.Active["llm"] = "qwen3.6-35b-a3b-iq4_nl_xl"
	plain := Render(base)
	if strings.Contains(plain, "defaultLlmModels") {
		t.Fatal("rendered without a list")
	}
	doc := strings.Replace(plain, `activeVideoModel = ""`, `activeVideoModel = ""`+"\n"+`defaultLlmModels = ["qwen3.6-35b-a3b-iq4_nl_xl","qwen3.5-4b-q4_k_m"]`, 1)
	if doc == plain {
		t.Fatalf("no activeVideoModel line in\n%s", plain)
	}
	desired, err := ParseDocument(doc, cat)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(desired.DefaultLLMs, []string{"qwen3.6-35b-a3b-iq4_nl_xl", "qwen3.5-4b-q4_k_m"}) {
		t.Fatalf("parsed %v", desired.DefaultLLMs)
	}
	next := Compose(base, desired)
	if got := Render(next); got != doc {
		t.Errorf("render:\n%s\nwant\n%s", got, doc)
	}
	plan, err := PlanChanges(base, desired, false, cat)
	if err != nil || len(plan.Changes) != 1 || plan.Changes[0].Path != "models.defaultLlmModels" || plan.Changes[0].Activation != "hot" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	// Removing the key clears the list.
	plan, _ = PlanChanges(next, base, false, cat)
	if len(plan.Changes) != 1 || plan.Changes[0].After != nil {
		t.Errorf("removal plan %+v", plan.Changes)
	}
	bad := map[string]string{
		`["qwen3.6-35b-a3b-iq4_nl_xl","qwen3.8-27b-q4_k_m"]`: "must also be present in selectedLlmModels",
		`["qwen3.6-35b-a3b-iq4_nl_xl","clef-flash-q4_k_m"]`:  "must name a chat model",
		`["qwen3.5-4b-q4_k_m","qwen3.5-4b-q4_k_m"]`:          "must not contain duplicates",
		`["qwen3.5-4b-q4_k_m"]`:                              "a single default must equal activeLlmModel",
		`[]`:                                                 "non-empty array",
	}
	for list, want := range bad {
		d := strings.Replace(plain, `activeVideoModel = ""`, `activeVideoModel = ""`+"\ndefaultLlmModels = "+list, 1)
		if _, err := ParseDocument(d, cat); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", list, err, want)
		}
	}
	// One entry equal to the active model is the active model alone.
	d := strings.Replace(plain, `activeVideoModel = ""`, `activeVideoModel = ""`+"\n"+`defaultLlmModels = ["qwen3.6-35b-a3b-iq4_nl_xl"]`, 1)
	if one, err := ParseDocument(d, cat); err != nil || one.DefaultLLMs != nil {
		t.Errorf("one entry: %v %v", one, err)
	}
}

func TestDefaultLLMsStored(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/local-base.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cat, _ := catalog.Load("")
	c := Default("/r", 64, cat)
	c.Selected["llm"] = []string{"qwen3.6-35b-a3b-iq4_nl_xl", "qwen3.5-4b-q4_k_m"}
	c.DefaultLLMs = []string{"qwen3.6-35b-a3b-iq4_nl_xl", "qwen3.5-4b-q4_k_m"}
	ctx := t.Context()
	if err := st.SaveConfig(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadConfig(ctx)
	if err != nil || !slices.Equal(got.DefaultLLMs, c.DefaultLLMs) {
		t.Fatalf("loaded %v %v", got.DefaultLLMs, err)
	}
	// Disabling a default drops it; one left is the active model alone.
	got.Selected["llm"] = []string{"qwen3.5-4b-q4_k_m"}
	st.SaveConfig(ctx, got)
	if again, _ := st.LoadConfig(ctx); again.DefaultLLMs != nil {
		t.Errorf("kept %v", again.DefaultLLMs)
	}
}
