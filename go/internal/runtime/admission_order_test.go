package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Characterization (DESIGN: observed > measured > feed > estimate): which evidence
// sizes a model, and what makes each kind not apply. Nothing failed before if this
// order changed. It pins today's behaviour, right or wrong.
func TestAdmissionDemandOrder(t *testing.T) {
	const (
		measured  = 2 * memory.GiB
		estimated = 3 * memory.GiB
		feedBytes = 5 * memory.GiB
		observed  = 7 * memory.GiB
	)
	launch := memory.LLMLaunch{CtxTokens: 8192, Slots: 1}
	shape := launch.LaunchShape()
	otherShape := memory.LLMLaunch{CtxTokens: 4096, Slots: 1}.LaunchShape()
	base, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var spec *catalog.Spec
	for _, s := range base.All() {
		if s.Kind == "llm" && s.Primary().SHA256 != "" && strings.HasPrefix(s.Source, "https://huggingface.co/") && s.RepositoryRevision != "" {
			spec = s
			break
		}
	}
	if spec == nil {
		t.Fatal("no catalog LLM with a repository and a hash")
	}
	sha := spec.Primary().SHA256
	repo := strings.TrimPrefix(spec.Source, "https://huggingface.co/")
	peak := func(launchShape string) catalog.MemoryPeak {
		return catalog.MemoryPeak{LaunchShape: launchShape, PeakBytes: feedBytes, BackendAcceleratorBytes: feedBytes, PeakRSSBytes: 1}
	}
	annotation := func(revision string, recipe *catalog.Recipe, peaks ...catalog.MemoryPeak) catalog.Annotation {
		return catalog.Annotation{Repo: repo, Revision: revision, File: spec.Primary().Filename, ModelID: spec.ModelID, MemoryPeaks: peaks, Recipe: recipe}
	}
	exact := annotation(spec.RepositoryRevision, nil, peak(shape))

	type obs struct {
		sha, shape, method string
	}
	good := obs{sha, shape, "footprint"}
	for _, c := range []struct {
		name      string
		observed  *obs
		feed      *catalog.Annotation
		planConf  string // what the plan already holds: measured or estimated
		modelFile string // "" is the catalog's primary file
		noSpec    bool
		want      string
	}{
		{name: "observed beats measured and feed", observed: &good, feed: &exact, planConf: "measured", want: "observed"},
		{name: "observed beats feed and estimate", observed: &good, feed: &exact, planConf: "estimated", want: "observed"},
		{name: "measured beats feed", feed: &exact, planConf: "measured", want: "measured"},
		{name: "feed beats estimate", feed: &exact, planConf: "estimated", want: "feed"},
		{name: "estimate alone", planConf: "estimated", want: "estimated"},
		{name: "measured alone", planConf: "measured", want: "measured"},

		{name: "observed at another shape does not apply", observed: &obs{sha, otherShape, "footprint"}, feed: &exact, planConf: "estimated", want: "feed"},
		{name: "observed for another file does not apply", observed: &obs{strings.Repeat("0", 64), shape, "footprint"}, planConf: "estimated", want: "estimated"},
		{name: "observed without a footprint does not apply", observed: &obs{sha, shape, "vmhwm"}, planConf: "estimated", want: "estimated"},

		{name: "feed at another shape does not apply", feed: ptr(annotation(spec.RepositoryRevision, nil, peak(otherShape))), planConf: "estimated", want: "estimated"},
		{name: "feed at another revision does not apply", feed: ptr(annotation(strings.Repeat("f", 40), nil, peak(shape))), planConf: "estimated", want: "estimated"},
		{name: "feed with another file's recipe does not apply", feed: ptr(annotation(spec.RepositoryRevision,
			&catalog.Recipe{Files: []catalog.RecipeFile{{Role: "primary", SHA256: strings.Repeat("0", 64)}}}, peak(shape))), planConf: "estimated", want: "estimated"},

		{name: "no evidence applies to another file", observed: &good, feed: &exact, planConf: "estimated", modelFile: "override.gguf", want: "estimated"},
		{name: "no evidence applies without a catalog entry", observed: &good, feed: &exact, planConf: "estimated", noSpec: true, want: "estimated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var remote *catalog.RemoteLayer
			if c.feed != nil {
				remote = &catalog.RemoteLayer{URL: "feed", Annotations: []catalog.Annotation{*c.feed}}
			}
			cat, err := catalog.LoadLayers("", remote)
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if c.observed != nil {
				err := st.RecordObservedPeak(context.Background(), store.ObservedPeak{ModelID: spec.ModelID, ArtifactSHA256: c.observed.sha,
					LaunchShape: c.observed.shape, PeakBytes: observed, Method: c.observed.method})
				if err != nil {
					t.Fatal(err)
				}
			}
			bytes := map[string]int64{"measured": measured, "estimated": estimated}[c.planConf]
			file := c.modelFile
			if file == "" {
				file = spec.Primary().Filename
			}
			plan := &LLMPlan{ModelPath: filepath.Join("/models", file), Launch: launch,
				ArtifactBytes: spec.TotalArtifactBytes(),
				Demand:        memory.Demand{UnifiedBytes: bytes, HostBytes: bytes, AcceleratorBytes: bytes, Confidence: c.planConf}}
			m := &LLM{d: LLMDeps{Store: st, Catalog: cat}}
			s := cat.ByID(spec.ModelID)
			if c.noSpec {
				s = nil
			}
			d := m.admissionDemand(s, plan)
			if d.Confidence != c.want {
				t.Fatalf("confidence %q, want %q (%d bytes)", d.Confidence, c.want, d.UnifiedBytes)
			}
			switch c.want {
			case "observed":
				if d.UnifiedBytes <= observed || d.UnifiedBytes != d.AcceleratorBytes {
					t.Errorf("observed should be the peak plus its margin: %+v", d)
				}
			case "feed":
				if d.AcceleratorBytes < feedBytes {
					t.Errorf("feed: %+v", d)
				}
			default:
				if d.UnifiedBytes != bytes {
					t.Errorf("should keep the plan's demand: %+v", d)
				}
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// A local file larger than the catalog's adds its extra to a feed figure, which
// measured the catalog's file.
func TestAdmissionDemandFeedAddsLocalExtra(t *testing.T) {
	base, _ := catalog.Load("")
	var spec *catalog.Spec
	for _, s := range base.All() {
		if s.Kind == "llm" && s.Primary().SHA256 != "" && strings.HasPrefix(s.Source, "https://huggingface.co/") && s.RepositoryRevision != "" {
			spec = s
			break
		}
	}
	an := catalog.Annotation{Repo: strings.TrimPrefix(spec.Source, "https://huggingface.co/"), Revision: spec.RepositoryRevision, File: spec.Primary().Filename,
		ModelID: spec.ModelID, MemoryPeaks: []catalog.MemoryPeak{{LaunchShape: memory.LLMLaunch{CtxTokens: 8192, Slots: 1}.LaunchShape(), PeakBytes: memory.GiB, BackendAcceleratorBytes: memory.GiB, PeakRSSBytes: 1}}}
	cat, err := catalog.LoadLayers("", &catalog.RemoteLayer{URL: "feed", Annotations: []catalog.Annotation{an}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "local-base.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := &LLM{d: LLMDeps{Store: st, Catalog: cat}}
	s := cat.ByID(spec.ModelID)
	demand := func(artifact int64) memory.Demand {
		return m.admissionDemand(s, &LLMPlan{ModelPath: filepath.Join("/models", spec.Primary().Filename), Launch: memory.LLMLaunch{CtxTokens: 8192, Slots: 1},
			ArtifactBytes: artifact, Demand: memory.Demand{Confidence: "estimated"}})
	}
	same, larger := demand(spec.TotalArtifactBytes()), demand(spec.TotalArtifactBytes()+memory.GiB)
	if same.Confidence != "feed" || larger.UnifiedBytes-same.UnifiedBytes != memory.GiB || larger.AcceleratorBytes-same.AcceleratorBytes != memory.GiB {
		t.Errorf("same %+v, larger %+v", same, larger)
	}
	if smaller := demand(spec.TotalArtifactBytes() - memory.GiB); smaller.UnifiedBytes != same.UnifiedBytes {
		t.Errorf("a smaller local file changed it: %+v", smaller)
	}
}
