package catalog

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
)

// The published feeds, built from the embedded catalog data (go run
// ./tools/catalog-feed writes them to catalog/ at the repository root):
//
//   - catalog/feed-v2.json: models plus annotations (status, capability marks,
//     measured memory peaks, multi-file recipes);
//   - catalog/feed.json: the models-only v1 document older binaries read.
//
// Former built-in entries stay in both feeds, so binaries built before they
// left the catalog keep finding them; their annotations say verified rather
// than qualified. Models are the data files as written, in order.txt order.

// FeedMeasurement is one checked-in memory measurement record
// (internal/memory/measurements.json), the source of the feed's memory peaks.
type FeedMeasurement struct {
	Kind    string `json:"kind"`
	ModelID string `json:"modelId"`
	Variant string `json:"variant"`
	Shape   struct {
		CtxTokens               int    `json:"ctxTokens"`
		Slots                   int    `json:"slots"`
		CacheTypeK              string `json:"cacheTypeK"`
		CacheTypeV              string `json:"cacheTypeV"`
		Width                   int    `json:"width"`
		Height                  int    `json:"height"`
		Steps                   int    `json:"steps"`
		Frames                  int    `json:"frames"`
		CPUOffload              bool   `json:"cpuOffload"`
		DiffusionFlashAttention bool   `json:"diffusionFlashAttention"`
	} `json:"shape"`
	BackendAcceleratorBytes int64  `json:"backendAcceleratorBytes"`
	HostBufferBytes         int64  `json:"hostBufferBytes"`
	PeakRSSBytes            int64  `json:"peakRssBytes"`
	SystemDeltaBytes        *int64 `json:"systemDeltaBytes"`
	Date                    string `json:"date"`
	Hardware                string `json:"hardware"`
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (m *FeedMeasurement) launchShape() string {
	s := m.Shape
	switch m.Kind {
	case "llm":
		return fmt.Sprintf("ctx=%d;slots=%d;kv=%s/%s", s.CtxTokens, s.Slots, s.CacheTypeK, s.CacheTypeV)
	case "stt":
		return "default"
	case "tts":
		return fmt.Sprintf("ctx=%d", s.CtxTokens)
	case "image":
		return fmt.Sprintf("%dx%d;steps=%d;diffusion-fa=%s", s.Width, s.Height, s.Steps, onOff(s.DiffusionFlashAttention))
	case "video":
		return fmt.Sprintf("%dx%d;frames=%d;cpu-offload=%s;diffusion-fa=%s", s.Width, s.Height, s.Frames,
			onOff(s.CPUOffload), onOff(s.DiffusionFlashAttention))
	}
	return ""
}

func (m *FeedMeasurement) peak() MemoryPeak {
	p := MemoryPeak{
		Variant:                 m.Variant,
		LaunchShape:             m.launchShape(),
		PeakBytes:               max(m.BackendAcceleratorBytes+m.HostBufferBytes, m.PeakRSSBytes),
		BackendAcceleratorBytes: m.BackendAcceleratorBytes,
		HostBufferBytes:         m.HostBufferBytes,
		PeakRSSBytes:            m.PeakRSSBytes,
		MeasuredAt:              m.Date,
		Hardware:                m.Hardware,
	}
	if m.SystemDeltaBytes != nil {
		p.PeakBytes = max(p.PeakBytes, *m.SystemDeltaBytes)
		delta := *m.SystemDeltaBytes
		p.SystemDeltaBytes = &delta
	}
	return p
}

// feedAnnotations is one annotation per entry whose primary file lives on
// Hugging Face (TS buildFeedAnnotations). status overrides the entry's own
// ("verified" for former entries); "" derives it.
func feedAnnotations(specs []*Spec, measurements []FeedMeasurement, status string) []Annotation {
	var out []Annotation
	for _, s := range specs {
		key, ok := s.primaryKey()
		if !ok {
			continue
		}
		a := Annotation{Repo: key.repo, Revision: key.revision, File: key.file, ModelID: s.ModelID, Kind: s.Kind, Status: status}
		if a.Status == "" {
			a.Status = "verified"
			if s.QualificationState == "qualified" && !s.VideoCatalogOnly {
				a.Status = "qualified"
			}
		}
		if len(s.UnsupportedCapabilities) > 0 {
			a.UnsupportedCapabilities = append([]Unsupported(nil), s.UnsupportedCapabilities...)
		}
		for i := range measurements {
			if measurements[i].ModelID == s.ModelID {
				a.MemoryPeaks = append(a.MemoryPeaks, measurements[i].peak())
			}
		}
		a.Recipe = recipeOf(s)
		out = append(out, a)
	}
	return out
}

// recipeOf is the install recipe of a multi-file entry whose every file is
// pinned (size and sha256), else nil.
func recipeOf(s *Spec) *Recipe {
	if len(s.Artifacts) < 2 {
		return nil
	}
	r := &Recipe{ModelID: s.ModelID}
	for _, a := range s.Artifacts {
		if a.SHA256 == "" || a.ExpectedSizeBytes == 0 {
			return nil
		}
		repoURL, rev := s.Source, s.RepositoryRevision
		if len(a.Source) > 0 {
			var src struct{ RepositoryURL, Revision string }
			if json.Unmarshal(a.Source, &src) == nil {
				if src.RepositoryURL != "" {
					repoURL = src.RepositoryURL
				}
				if src.Revision != "" {
					rev = src.Revision
				}
			}
		}
		r.Files = append(r.Files, RecipeFile{
			RepositoryURL: repoURL,
			Revision:      strings.ToLower(rev),
			Path:          a.SourcePath,
			SizeBytes:     a.ExpectedSizeBytes,
			SHA256:        strings.ToLower(a.SHA256),
			Role:          a.Role,
		})
		r.TotalBytes += a.ExpectedSizeBytes
	}
	return r
}

// readFeedData is the data files under dir (in its order.txt order), parsed
// as written and as specs.
func readFeedData(dir string) (models []any, specs []*Spec, err error) {
	order, err := builtInFS.ReadFile(dir + "/order.txt")
	if err != nil {
		return nil, nil, err
	}
	for _, rel := range strings.Fields(string(order)) {
		b, err := builtInFS.ReadFile(dir + "/" + rel)
		if err != nil {
			return nil, nil, err
		}
		v, err := jsjson.Parse(b)
		if err != nil {
			return nil, nil, fmt.Errorf("%s/%s: %w", dir, rel, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, nil, fmt.Errorf("%s/%s: %w", dir, rel, err)
		}
		s, err := decodeStrict(m)
		if err != nil {
			return nil, nil, fmt.Errorf("%s/%s: %w", dir, rel, err)
		}
		models = append(models, v)
		specs = append(specs, s)
	}
	return models, specs, nil
}

// RenderFeeds builds catalog/feed.json (v1) and catalog/feed-v2.json from the
// embedded catalog data and the memory measurement records (the bytes of
// internal/memory/measurements.json), each as JSON.stringify(feed, null, 2)
// plus a newline.
func RenderFeeds(measurementsJSON []byte) (v1, v2 []byte, err error) {
	var measurements []FeedMeasurement
	if err := json.Unmarshal(measurementsJSON, &measurements); err != nil {
		return nil, nil, fmt.Errorf("measurements: %w", err)
	}
	models, specs, err := readFeedData("data")
	if err != nil {
		return nil, nil, err
	}
	formerModels, formerSpecs, err := readFeedData("data/former")
	if err != nil {
		return nil, nil, err
	}
	models = append(models, formerModels...)
	annotations := append([]Annotation{}, feedAnnotations(specs, measurements, "")...)
	annotations = append(annotations, feedAnnotations(formerSpecs, measurements, "verified")...)

	legacy := jsjson.NewObject()
	legacy.Set("format", FeedFormat)
	legacy.Set("schemaVersion", float64(FeedLegacyVersion))
	legacy.Set("sha256", feedChecksum(FeedLegacyVersion, modelsOnly(models)))
	legacy.Set("models", models)

	body := jsjson.NewObject()
	body.Set("models", models)
	body.Set("annotations", jsjson.FromGo(annotations))
	current := jsjson.NewObject()
	current.Set("format", FeedFormat)
	current.Set("schemaVersion", float64(FeedSchemaVersion))
	current.Set("minReaderVersion", float64(FeedSchemaVersion))
	current.Set("sha256", feedChecksum(FeedSchemaVersion, body))
	for _, k := range body.Keys() {
		v, _ := body.Get(k)
		current.Set(k, v)
	}
	return render(legacy), render(current), nil
}

func modelsOnly(models []any) *jsjson.Object {
	o := jsjson.NewObject()
	o.Set("models", models)
	return o
}

func render(v any) []byte { return append(jsjson.Indent(v), '\n') }
