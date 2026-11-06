// Package models holds logical models: one identity with one or more
// executable variants, each a set of artifacts plus facts about them. Every
// fact names where it came from; a fact nobody established stays unknown.
// Records here describe models only: which backend runs a model is decided
// per device by package backends.
package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Fact sources, strongest last.
const (
	SourcePinned   = "pinned-metadata"  // a pinned upstream record (catalog entry, HF API at a commit)
	SourceDeclared = "declared"         // stated by the model's author or the person importing it
	SourceInspect  = "inspected"        // read from the artifact's own header
	SourceVerified = "verified-runtime" // reported by a backend that loaded it
)

var sourceRank = map[string]int{SourcePinned: 1, SourceDeclared: 1, SourceInspect: 2, SourceVerified: 3}

// Fact is a value with its evidence. The zero Fact is unknown.
type Fact[T comparable] struct {
	Value  T      `json:"value"`
	Source string `json:"source"`
	// Conflict records a disagreeing value from another source; the fact is
	// then unknown.
	Conflict string `json:"conflict,omitempty"`
}

// Known reports whether the fact has a value.
func (f Fact[T]) Known() bool { return f.Source != "" && f.Conflict == "" }

// Of returns a known fact.
func Of[T comparable](v T, source string) Fact[T] { return Fact[T]{Value: v, Source: source} }

// Merge combines evidence: equal values keep the stronger source; different
// values from the same strength make the fact unknown (both recorded); a
// stronger source (inspection or runtime evidence about the actual file)
// replaces a weaker one.
func Merge[T comparable](a, b Fact[T]) Fact[T] {
	switch {
	case b.Source == "":
		return a
	case a.Source == "":
		return b
	case a.Value == b.Value:
		if sourceRank[b.Source] > sourceRank[a.Source] {
			return b
		}
		return a
	case sourceRank[b.Source] > sourceRank[a.Source]:
		// The file itself says something else than the record: the file wins.
		return b
	}
	return Fact[T]{Source: a.Source, Conflict: fmt.Sprintf("%v (%s) vs %v (%s)", a.Value, a.Source, b.Value, b.Source)}
}

// Artifact roles.
const (
	RoleWeights     = "weights"
	RoleProjector   = "projector" // vision/audio encoder for a chat model
	RoleTokenizer   = "tokenizer"
	RoleConfig      = "config"
	RoleVAE         = "vae"
	RoleTextEncoder = "text-encoder"
	RoleVoice       = "voice"
	RoleCompanion   = "companion" // other required data file
)

// Hash trust: how much a sha256 says.
const (
	HashAuthoritative = "authoritative" // from the publisher's pinned record (catalog, HF LFS pointer)
	HashUser          = "user"          // typed by the person importing; identity against their value
	HashObserved      = "observed"      // computed after download; identity only, not publisher verification
)

// Source is where an artifact is acquired.
type Source struct {
	Kind        string `json:"kind"` // huggingface | url | local
	Repo        string `json:"repo,omitempty"`
	Revision    string `json:"revision,omitempty"` // HF: immutable commit
	Path        string `json:"path,omitempty"`     // HF file path, local path
	URL         string `json:"url,omitempty"`      // direct URL; never contains credentials
	AuthProfile string `json:"authProfile,omitempty"`
}

// Custom reports a direct-URL source, which never gains curated status.
func (s Source) Custom() bool { return s.Kind == "url" }

// Artifact is one file of a variant.
type Artifact struct {
	Filename  string `json:"filename"`
	Role      string `json:"role"`
	Required  bool   `json:"required"`
	SizeBytes int64  `json:"sizeBytes,omitempty"` // expected, 0 = unknown
	SHA256    string `json:"sha256,omitempty"`
	HashTrust string `json:"hashTrust,omitempty"`
	Source    Source `json:"source"`
}

// Variant is one executable form of a model.
type Variant struct {
	ID            string       `json:"id"`
	Format        Fact[string] `json:"format"`       // gguf | safetensors | onnx | ggml | mlx
	Architecture  Fact[string] `json:"architecture"` // e.g. qwen3, llama, whisper, flux
	Precision     Fact[string] `json:"precision"`    // e.g. Q4_K_M, F16, BF16
	ContextLength Fact[int]    `json:"contextLength"`
	Dimensions    Fact[int]    `json:"dimensions"`
	Artifacts     []Artifact   `json:"artifacts"`
	// Backends the publisher of this record qualified the variant on, with
	// the fact source of that claim.
	QualifiedOn []string `json:"qualifiedOn,omitempty"`
	// MinBackendVersion: a backend id -> minimum version this variant needs.
	MinBackendVersion map[string]string `json:"minBackendVersion,omitempty"`
}

// Model is a logical model.
type Model struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Tasks the model is for: chat, embeddings, decision, image, video,
	// speech (TTS), transcription (ASR).
	Tasks []string `json:"tasks"`
	// Capabilities beyond the task, which a backend must keep: vision,
	// audio-input, tools, long-context, ...
	Capabilities []string  `json:"capabilities"`
	Variants     []Variant `json:"variants"`
	Origin       string    `json:"origin"` // catalog | user-catalog | import
	License      string    `json:"license,omitempty"`
	SourceURL    string    `json:"sourceUrl,omitempty"`
	CreatedAt    string    `json:"createdAt,omitempty"`
}

// Tasks.
var Tasks = []string{"chat", "embeddings", "decision", "image", "video", "speech", "transcription"}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ValidID reports a model or variant id.
func ValidID(s string) bool { return idRe.MatchString(s) }

// Validate checks the structure, not the facts.
func (m *Model) Validate() error {
	if !ValidID(m.ID) {
		return fmt.Errorf("model id %q must be lowercase letters, digits, '.', '_' or '-'", m.ID)
	}
	if len(m.Tasks) == 0 {
		return errors.New("a model needs at least one task")
	}
	for _, t := range m.Tasks {
		if !slices.Contains(Tasks, t) {
			return fmt.Errorf("unknown task %q", t)
		}
	}
	if len(m.Variants) == 0 {
		return errors.New("a model needs at least one variant")
	}
	seen := map[string]bool{}
	for _, v := range m.Variants {
		if !ValidID(v.ID) || seen[v.ID] {
			return fmt.Errorf("variant id %q is invalid or repeated", v.ID)
		}
		seen[v.ID] = true
		if len(v.Artifacts) == 0 {
			return fmt.Errorf("variant %s has no artifacts", v.ID)
		}
		names := map[string]bool{}
		for _, a := range v.Artifacts {
			if a.Filename == "" || strings.ContainsAny(a.Filename, `/\`) || a.Filename == "." || a.Filename == ".." || names[a.Filename] {
				return fmt.Errorf("variant %s: artifact filename %q is invalid or repeated", v.ID, a.Filename)
			}
			names[a.Filename] = true
			if a.Source.URL != "" && strings.Contains(a.Source.URL, "@") {
				return fmt.Errorf("variant %s: URLs must not embed credentials; use an auth profile", v.ID)
			}
		}
	}
	return nil
}

// Variant returns a variant by id.
func (m *Model) Variant(id string) *Variant {
	for i := range m.Variants {
		if m.Variants[i].ID == id {
			return &m.Variants[i]
		}
	}
	return nil
}

// HasTask reports a task.
func (m *Model) HasTask(t string) bool { return slices.Contains(m.Tasks, t) }

// Required returns the capabilities a backend must keep: tasks plus
// capabilities.
func (m *Model) Required() []string {
	return append(append([]string{}, m.Tasks...), m.Capabilities...)
}

// TotalBytes sums the known artifact sizes; unknown reports whether any size
// is unknown.
func (v *Variant) TotalBytes() (total int64, unknown bool) {
	for _, a := range v.Artifacts {
		if a.SizeBytes <= 0 {
			unknown = true
		}
		total += a.SizeBytes
	}
	return total, unknown
}

// Export is the model record for sharing: device-local state (selections)
// never lives in a Model, and sources keep only auth-profile ids.
func (m *Model) Export() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}
