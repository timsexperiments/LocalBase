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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
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
	ModelID                 string              `json:"modelId"`
	QualificationState      string              `json:"qualificationState,omitempty"`
	Kind                    string              `json:"kind"`
	Provider                string              `json:"provider"`
	DisplayName             string              `json:"displayName,omitempty"`
	Family                  string              `json:"family"`
	Version                 string              `json:"version"`
	Size                    string              `json:"size"`
	Quant                   string              `json:"quant"`
	CodingScore             *float64            `json:"codingScore,omitempty"`
	StorageGB               float64             `json:"storageGb"`
	Source                  string              `json:"source"`
	RepositoryRevision      string              `json:"repositoryRevision"`
	Artifacts               []Artifact          `json:"artifacts"`
	LLMRuntime              *LLMRuntime         `json:"llmRuntime,omitempty"`
	TTSRuntime              *TTSRuntime         `json:"ttsRuntime,omitempty"`
	ImageRuntime            *ImageRuntime       `json:"imageRuntime,omitempty"`
	ImageLaunchOptions      *ImageLaunchOptions `json:"imageLaunchOptions,omitempty"`
	VideoRuntime            *VideoRuntime       `json:"videoRuntime,omitempty"`
	VideoCatalogOnly        bool                `json:"videoCatalogOnly,omitempty"`
	InputModalities         []string            `json:"inputModalities"`
	OutputModalities        []string            `json:"outputModalities"`
	ContextWindowTokens     *int                `json:"contextWindowTokens,omitempty"`
	Capabilities            []Capability        `json:"capabilities"`
	UnsupportedCapabilities []Unsupported       `json:"unsupportedCapabilities,omitempty"`
	OutputFormat            string              `json:"outputFormat,omitempty"`
	ExperimentalRuntime     bool                `json:"experimentalRuntime,omitempty"`
	DiffusionWeightsOnly    bool                `json:"diffusionWeightsOnly,omitempty"`
	CommercialStatus        string              `json:"commercialStatus"`
	Catch                   string              `json:"catch"`
	Notes                   string              `json:"notes"`

	// Trust is qualified (built-in), community (remote feed), user (new user
	// entry) or modified (a patch changed how it launches). Origin is the tier
	// the entry came from, Tier the highest tier that contributed, LayerPath
	// that tier's file or feed position. Not part of the JSON.
	Trust     string `json:"-"`
	Origin    string `json:"-"`
	Tier      string `json:"-"`
	LayerPath string `json:"-"`
	// PatchedFields are the top-level fields a higher tier changed, in
	// schema order.
	PatchedFields []string `json:"-"`
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
	Tier    string // remote | user
	Path    string
	ModelID string
	Message string
}

// Catalog is the effective catalog. A gateway replaces its contents in place
// when the remote feed updates (Replace), so every holder sees the new one;
// read Models through All.
type Catalog struct {
	mu          sync.RWMutex
	Models      []*Spec
	byID        map[string]*Spec
	Diagnostics []Diagnostic
	// Annotations are the feed annotations in force.
	Annotations []Annotation
	byRepo      map[string][]int
}

func (c *Catalog) ByID(id string) *Spec {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byID[id]
}

// All lists the models in catalog order.
func (c *Catalog) All() []*Spec {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Models
}

// Replace swaps in a reloaded catalog.
func (c *Catalog) Replace(next *Catalog) {
	next.mu.RLock()
	models, byID, diags, ann, byRepo := next.Models, next.byID, next.Diagnostics, next.Annotations, next.byRepo
	next.mu.RUnlock()
	c.mu.Lock()
	c.Models, c.byID, c.Diagnostics, c.Annotations, c.byRepo = models, byID, diags, ann, byRepo
	c.mu.Unlock()
}

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

// RemoteLayer is the downloaded community feed: its entries (built-in ids
// already left out) and the annotations it publishes.
type RemoteLayer struct {
	URL         string
	Entries     []json.RawMessage
	Annotations []Annotation
}

// Load reads the built-in catalog and the user layer under userDir (may be "").
func Load(userDir string) (*Catalog, error) { return LoadLayers(userDir, nil) }

// specKeyOrder is specKeys in schema order (TS SPEC_KEYS).
var specKeyOrder = func() []string {
	var out []string
	t := reflect.TypeOf(Spec{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}()

// specKeys are the top-level fields a catalog entry may set.
var specKeys = func() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(Spec{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}()

type layered struct {
	raw                       map[string]json.RawMessage
	trust, origin, tier, path string
	patched                   []string
}

// LoadLayers composes the built-in catalog, the remote layer (nil when the
// feed is off or not downloaded) and the user layer, lowest first (TS
// composeCatalog). A higher tier wins; an existing modelId is a shallow
// patch. Invalid entries are skipped and reported in Diagnostics.
func LoadLayers(userDir string, remote *RemoteLayer) (*Catalog, error) {
	order, err := builtInFS.ReadFile("data/order.txt")
	if err != nil {
		return nil, err
	}
	entries := map[string]*layered{}
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
		entries[id] = &layered{raw: m, trust: "qualified", origin: "built-in", tier: "built-in"}
		ids = append(ids, id)
	}
	c := &Catalog{byID: map[string]*Spec{}}
	if remote != nil {
		seen := map[string]string{}
		for i, data := range remote.Entries {
			path := fmt.Sprintf("%s#models[%d]", remote.URL, i)
			report := func(modelID, msg string) {
				c.Diagnostics = append(c.Diagnostics, Diagnostic{Tier: "remote", Path: path, ModelID: modelID, Message: msg})
			}
			var m map[string]json.RawMessage
			if json.Unmarshal(data, &m) != nil || m == nil {
				report("", "The file must contain one JSON object (a model spec).")
				continue
			}
			var id string
			if json.Unmarshal(m["modelId"], &id) != nil || id == "" {
				report("", `The entry needs a non-empty "modelId".`)
				continue
			}
			if base, ok := entries[id]; ok && base.origin == "built-in" {
				continue // the binary's own entry wins; the caller filters these too
			}
			var unknown []string
			for _, k := range jsonKeys(data) {
				if !specKeys[k] {
					unknown = append(unknown, k)
				}
			}
			if len(unknown) > 0 {
				report(id, "Unknown field(s): "+strings.Join(unknown, ", ")+".")
				continue
			}
			if earlier, dup := seen[id]; dup {
				report(id, fmt.Sprintf("Duplicate entry for %q; %s wins.", id, earlier))
				continue
			}
			if _, err := decodeStrict(m); err != nil {
				report(id, "The entry is invalid: "+err.Error())
				continue
			}
			seen[id] = path
			entries[id] = &layered{raw: m, trust: "community", origin: "remote", tier: "remote", path: path}
			ids = append(ids, id)
		}
		c.Annotations = remote.Annotations
	}
	if userDir != "" {
		files, fileDiags := readUserLayer(userDir)
		c.Diagnostics = append(c.Diagnostics, fileDiags...)
		seen := map[string]string{}
		for _, f := range files {
			path := f.path
			report := func(modelID, msg string) {
				c.Diagnostics = append(c.Diagnostics, Diagnostic{Tier: "user", Path: path, ModelID: modelID, Message: msg})
			}
			var m map[string]json.RawMessage
			if json.Unmarshal(f.data, &m) != nil || m == nil {
				report("", "The file must contain one JSON object (a model spec).")
				continue
			}
			var id string
			if json.Unmarshal(m["modelId"], &id) != nil || id == "" {
				report("", `The entry needs a non-empty "modelId".`)
				continue
			}
			var unknown []string
			for _, k := range jsonKeys(f.data) {
				if !specKeys[k] {
					unknown = append(unknown, k)
				}
			}
			if len(unknown) > 0 {
				report(id, "Unknown field(s): "+strings.Join(unknown, ", ")+".")
				continue
			}
			if earlier, dup := seen[id]; dup {
				report(id, fmt.Sprintf("Duplicate entry for %q; %s wins.", id, earlier))
				continue
			}
			base, exists := entries[id]
			merged := map[string]json.RawMessage{}
			if exists {
				for k, v := range base.raw {
					merged[k] = v
				}
			}
			changedLaunch := false
			changed := map[string]bool{}
			for k, v := range m {
				if exists && !sameJSON(base.raw[k], v) {
					changed[k] = true
					changedLaunch = changedLaunch || launchShaped[k]
				}
				merged[k] = v
			}
			if _, err := decodeStrict(merged); err != nil {
				prefix := "The entry is invalid: "
				if exists {
					prefix = "The patched entry is invalid: "
				}
				report(id, prefix+describeDecodeError(err))
				continue
			}
			seen[id] = path
			if !exists {
				entries[id] = &layered{raw: merged, trust: "user", origin: "user", tier: "user", path: path}
				ids = append(ids, id)
				continue
			}
			next := *base
			next.raw, next.tier, next.path = merged, "user", path
			next.patched = nil
			for _, k := range specKeyOrder {
				if changed[k] || slices.Contains(base.patched, k) {
					next.patched = append(next.patched, k)
				}
			}
			if changedLaunch && (base.trust == "qualified" || base.trust == "community") {
				next.trust = "modified"
			}
			entries[id] = &next
		}
	}
	for _, id := range ids {
		e := entries[id]
		s, err := decodeStrict(e.raw)
		if err != nil {
			return nil, fmt.Errorf("catalog entry %s: %w", id, err)
		}
		s.Trust, s.Origin, s.Tier, s.LayerPath = e.trust, e.origin, e.tier, e.path
		s.PatchedFields = append([]string{}, e.patched...)
		c.Models = append(c.Models, s)
		c.byID[id] = s
	}
	c.indexAnnotations()
	return c, nil
}

// jsonKeys lists an object's keys in JavaScript order (Object.keys).
func jsonKeys(b []byte) []string {
	v, err := jsjson.Parse(b)
	if err != nil {
		return nil
	}
	if o, ok := v.(*jsjson.Object); ok {
		return o.Keys()
	}
	return nil
}

func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// Installation reports whether every artifact is in dir at its expected size
// (no hashing), and the bytes found.
func (s *Spec) Installation(dir string) (complete bool, bytes int64) {
	complete = true
	for _, a := range s.Artifacts {
		info, err := os.Stat(filepath.Join(dir, a.Filename))
		if err != nil || !info.Mode().IsRegular() || (a.ExpectedSizeBytes > 0 && info.Size() != a.ExpectedSizeBytes) {
			complete = false
			continue
		}
		bytes += info.Size()
	}
	return complete, bytes
}

// Projector is an LLM's vision/audio projector artifact, if it ships one.
func (s *Spec) Projector() *Artifact {
	if s.Kind != "llm" {
		return nil
	}
	for i := range s.Artifacts {
		a := &s.Artifacts[i]
		if a.Role == "supplementary" && strings.Contains(strings.ToLower(a.Filename), "mmproj") {
			return a
		}
	}
	return nil
}

type userFile struct {
	path string
	data json.RawMessage
}

// readUserLayer reads catalog.d/*.json in name order (TS
// loadUserCatalogLayer); files it cannot use are reported, not fatal.
func readUserLayer(dir string) ([]userFile, []Diagnostic) {
	names, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, nil
		}
		return nil, []Diagnostic{{Tier: "user", Path: dir, Message: "The user catalog directory could not be read: " + err.Error()}}
	}
	var files []userFile
	var diags []Diagnostic
	for _, n := range names {
		if !strings.HasSuffix(n.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, n.Name())
		st, err := os.Lstat(path)
		switch {
		case err != nil:
			diags = append(diags, Diagnostic{Tier: "user", Path: path, Message: "Skipped: unreadable (" + err.Error() + ")."})
			continue
		case !st.Mode().IsRegular():
			diags = append(diags, Diagnostic{Tier: "user", Path: path, Message: "Skipped: not a regular file."})
			continue
		case st.Size() > 1<<20:
			diags = append(diags, Diagnostic{Tier: "user", Path: path, Message: "Skipped: larger than 1 MiB."})
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			diags = append(diags, Diagnostic{Tier: "user", Path: path, Message: "Skipped: unreadable (" + err.Error() + ")."})
			continue
		}
		if !json.Valid(b) {
			var v any
			msg := "invalid JSON"
			if err := json.Unmarshal(b, &v); err != nil {
				msg += " (" + err.Error() + ")"
			}
			diags = append(diags, Diagnostic{Tier: "user", Path: path, Message: "Skipped: " + msg + "."})
			continue
		}
		files = append(files, userFile{path, b})
	}
	return files, diags
}

// describeDecodeError words a decoding failure as TS describes a zod issue
// where the cause is a field of the wrong JSON type.
func describeDecodeError(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		expected := "object"
		switch te.Type.Kind() {
		case reflect.String:
			expected = "string"
		case reflect.Bool:
			expected = "boolean"
		case reflect.Float32, reflect.Float64, reflect.Int, reflect.Int64, reflect.Int32:
			expected = "number"
		case reflect.Slice, reflect.Array:
			expected = "array"
		}
		received := map[string]string{"bool": "boolean"}[te.Value]
		if received == "" {
			received = te.Value
		}
		return te.Field + ": Invalid input: expected " + expected + ", received " + received
	}
	return err.Error()
}
