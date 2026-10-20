// Package catalog loads model specs: the built-in catalog embedded in the
// binary, then user entries from `<root>/catalog.d/`. The JSON format is the
// TS `modelSpecObjectSchema`, decoded strictly.
//
// Layering (lowest first): built-in, remote feed (milestone 5), user. A new
// modelId adds a model; an existing one is a shallow patch where each
// top-level key replaces the lower value whole.
package catalog

//go:generate ../../tools/sync-catalog.sh

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

//go:embed data
var builtInFS embed.FS

type Artifact struct {
	SourcePath        string          `json:"sourcePath"`
	Filename          string          `json:"filename"`
	ExpectedSizeBytes int64           `json:"expectedSizeBytes,omitempty"`
	SHA256            string          `json:"sha256,omitempty"`
	Role              string          `json:"role"` // primary | supplementary
	Source            json.RawMessage `json:"source,omitempty"`
}

type LLMRuntime struct {
	Capability string          `json:"capability"` // embedding-only | decision-only
	Pooling    string          `json:"pooling,omitempty"`
	Dimensions json.RawMessage `json:"dimensions,omitempty"`
}

// Capability accepts the catalog's two input forms: "id" or {"id","source"}.
type Capability struct {
	ID     string `json:"id"`
	Source string `json:"source"`
}

func (c *Capability) UnmarshalJSON(b []byte) error {
	var id string
	if err := json.Unmarshal(b, &id); err == nil {
		*c = Capability{ID: id, Source: "declared"}
		return nil
	}
	type plain Capability
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode((*plain)(c))
}

type Unsupported struct {
	ID        string `json:"id"`
	Supported bool   `json:"supported"`
	Source    string `json:"source"`
	Verified  bool   `json:"verified"`
	Reason    string `json:"reason"`
}

// Spec is one catalog model. Runtime profiles the skeleton does not use yet
// stay raw so they round-trip unchanged.
type Spec struct {
	ModelID                 string          `json:"modelId"`
	QualificationState      string          `json:"qualificationState,omitempty"`
	Kind                    string          `json:"kind"`
	Provider                string          `json:"provider"`
	DisplayName             string          `json:"displayName,omitempty"`
	Family                  string          `json:"family"`
	Version                 string          `json:"version"`
	Size                    string          `json:"size"`
	Quant                   string          `json:"quant"`
	CodingScore             *float64        `json:"codingScore,omitempty"`
	StorageGB               float64         `json:"storageGb"`
	Source                  string          `json:"source"`
	RepositoryRevision      string          `json:"repositoryRevision"`
	Artifacts               []Artifact      `json:"artifacts"`
	LLMRuntime              *LLMRuntime     `json:"llmRuntime,omitempty"`
	TTSRuntime              json.RawMessage `json:"ttsRuntime,omitempty"`
	ImageRuntime            json.RawMessage `json:"imageRuntime,omitempty"`
	ImageLaunchOptions      json.RawMessage `json:"imageLaunchOptions,omitempty"`
	VideoRuntime            json.RawMessage `json:"videoRuntime,omitempty"`
	VideoCatalogOnly        bool            `json:"videoCatalogOnly,omitempty"`
	InputModalities         []string        `json:"inputModalities"`
	OutputModalities        []string        `json:"outputModalities"`
	ContextWindowTokens     *int            `json:"contextWindowTokens,omitempty"`
	Capabilities            []Capability    `json:"capabilities"`
	UnsupportedCapabilities []Unsupported   `json:"unsupportedCapabilities,omitempty"`
	OutputFormat            string          `json:"outputFormat,omitempty"`
	ExperimentalRuntime     bool            `json:"experimentalRuntime,omitempty"`
	DiffusionWeightsOnly    bool            `json:"diffusionWeightsOnly,omitempty"`
	CommercialStatus        string          `json:"commercialStatus"`
	Catch                   string          `json:"catch"`
	Notes                   string          `json:"notes"`

	// Trust is qualified (built-in), user (new user entry) or modified (a
	// patch changed how it launches). Not part of the JSON.
	Trust string `json:"-"`
}

func (s *Spec) Name() string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Family
}

func (s *Spec) Primary() *Artifact {
	for i := range s.Artifacts {
		if s.Artifacts[i].Role == "primary" {
			return &s.Artifacts[i]
		}
	}
	return &s.Artifacts[0]
}

func (s *Spec) TotalArtifactBytes() int64 {
	var n int64
	for _, a := range s.Artifacts {
		n += a.ExpectedSizeBytes
	}
	return n
}

func (s *Spec) IsDecision() bool {
	return s.LLMRuntime != nil && s.LLMRuntime.Capability == "decision-only"
}
func (s *Spec) IsEmbedding() bool {
	return s.LLMRuntime != nil && s.LLMRuntime.Capability == "embedding-only"
}

func (s *Spec) HasCapability(id string) bool {
	for _, c := range s.Capabilities {
		if c.ID == id {
			return true
		}
	}
	return false
}

// Qualified means the checked-in memory measurements apply.
func (s *Spec) Qualified() bool { return s.Trust == "qualified" }

// Diagnostic reports a skipped catalog file; it never stops startup.
type Diagnostic struct {
	Path    string
	Message string
}

type Catalog struct {
	Models      []*Spec
	byID        map[string]*Spec
	Diagnostics []Diagnostic
}

func (c *Catalog) ByID(id string) *Spec { return c.byID[id] }

var launchShaped = map[string]bool{
	"kind": true, "source": true, "repositoryRevision": true, "artifacts": true,
	"llmRuntime": true, "ttsRuntime": true, "imageRuntime": true,
	"imageLaunchOptions": true, "videoRuntime": true, "contextWindowTokens": true,
}

func decodeStrict(raw map[string]json.RawMessage) (*Spec, error) {
	b, _ := json.Marshal(raw)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Spec) validate() error {
	switch {
	case s.ModelID == "":
		return fmt.Errorf("modelId is required")
	case !map[string]bool{"llm": true, "stt": true, "tts": true, "image": true, "video": true}[s.Kind]:
		return fmt.Errorf("kind %q is invalid", s.Kind)
	case len(s.Artifacts) == 0:
		return fmt.Errorf("artifacts must not be empty")
	case len(s.InputModalities) == 0 || len(s.OutputModalities) == 0:
		return fmt.Errorf("modalities must not be empty")
	}
	primaries := 0
	names := map[string]bool{}
	for _, a := range s.Artifacts {
		if a.Filename == "" || strings.ContainsAny(a.Filename, "/\\") || a.Filename == "." || a.Filename == ".." {
			return fmt.Errorf("artifact filename %q is unsafe", a.Filename)
		}
		if names[a.Filename] {
			return fmt.Errorf("artifact filenames must be unique")
		}
		names[a.Filename] = true
		if a.Role == "primary" {
			primaries++
		}
	}
	if primaries != 1 {
		return fmt.Errorf("exactly one primary artifact is required")
	}
	if s.QualificationState == "" {
		s.QualificationState = "qualified"
	}
	return nil
}

// Load reads the built-in catalog and the user layer under userDir (may be "").
func Load(userDir string) (*Catalog, error) {
	order, err := builtInFS.ReadFile("data/order.txt")
	if err != nil {
		return nil, err
	}
	raws := map[string]map[string]json.RawMessage{}
	trust := map[string]string{}
	var ids []string
	for _, rel := range strings.Fields(string(order)) {
		b, err := builtInFS.ReadFile("data/" + rel)
		if err != nil {
			return nil, err
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("built-in %s: %w", rel, err)
		}
		var id string
		json.Unmarshal(m["modelId"], &id)
		raws[id] = m
		trust[id] = "qualified"
		ids = append(ids, id)
	}
	c := &Catalog{byID: map[string]*Spec{}}
	if userDir != "" {
		files, _ := filepath.Glob(filepath.Join(userDir, "*.json"))
		sort.Strings(files)
		for _, path := range files {
			b, err := os.ReadFile(path)
			if err != nil {
				c.Diagnostics = append(c.Diagnostics, Diagnostic{path, err.Error()})
				continue
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(b, &m); err != nil {
				c.Diagnostics = append(c.Diagnostics, Diagnostic{path, "the file must contain one JSON object: " + err.Error()})
				continue
			}
			var id string
			if json.Unmarshal(m["modelId"], &id) != nil || id == "" {
				c.Diagnostics = append(c.Diagnostics, Diagnostic{path, "modelId is required"})
				continue
			}
			base, exists := raws[id]
			merged := map[string]json.RawMessage{}
			for k, v := range base {
				merged[k] = v
			}
			changedLaunch := false
			for k, v := range m {
				if exists && launchShaped[k] && !sameJSON(base[k], v) {
					changedLaunch = true
				}
				merged[k] = v
			}
			if _, err := decodeStrict(merged); err != nil {
				c.Diagnostics = append(c.Diagnostics, Diagnostic{path, err.Error()})
				continue
			}
			raws[id] = merged
			switch {
			case !exists:
				trust[id] = "user"
				ids = append(ids, id)
			case changedLaunch:
				trust[id] = "modified"
			}
		}
	}
	for _, id := range ids {
		s, err := decodeStrict(raws[id])
		if err != nil {
			return nil, fmt.Errorf("catalog entry %s: %w", id, err)
		}
		s.Trust = trust[id]
		c.Models = append(c.Models, s)
		c.byID[id] = s
	}
	return c, nil
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
