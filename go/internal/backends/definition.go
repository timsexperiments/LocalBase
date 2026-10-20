// Package backends is the backend registry: versioned definitions of the
// engines LocalBase can run models with, the host they run on, a best-effort
// detector that rates a model variant against each backend, and explicit
// routing rules that turn ratings into a recommendation. Detection informs;
// it never hides a backend or overrides a person's choice.
package backends

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/timsexperiments/LocalBase/go/internal/models"
)

// Schema is the definition format version this build reads.
const Schema = "localbase.backend/v1"

// Adapter interfaces this build implements. A definition names one; custom
// definitions may use any of them with their own executable.
var Adapters = []string{
	"llama-server/v1", "sd-server/v1", "whisper-server/v1", "tts-server/v1", "kokoro-onnx/v1", "crispasr/v1",
	"process-openai/v1",
}

// Platform is one os/arch/accelerator a backend build runs on.
type Platform struct {
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Accelerator string `json:"accelerator"` // metal | cuda | vulkan | cpu | ...
}

// Architectures rates model architectures for a backend.
type Architectures struct {
	Supported   []string `json:"supported,omitempty"`   // loaded and qualified with this backend
	Likely      []string `json:"likely,omitempty"`      // documented to work, not qualified here
	Unsupported []string `json:"unsupported,omitempty"` // known not to work
}

// Runtime says where a built-in backend's executable comes from.
type Runtime struct {
	Family     string `json:"family"`     // managed runtime package
	Executable string `json:"executable"` // file inside it
	Managed    bool   `json:"managed"`    // a pinned package exists for this platform
}

// Memory is a declared admission estimate for a custom backend. Assumption
// says, in words, what the numbers cover; it is shown with every estimate.
type Memory struct {
	WeightsFactor float64 `json:"weightsFactor"` // demand = weights × factor + overhead
	OverheadGB    float64 `json:"overheadGb"`
	Assumption    string  `json:"assumption"`
}

// Launch is how a custom backend starts (adapter process-openai/v1, or a
// built-in adapter with a user-supplied executable).
type Launch struct {
	Command    string   `json:"command"` // absolute path to an existing executable
	Args       []string `json:"args"`    // templates: {model} {model_dir} {projector} {host} {port} {ctx}
	HealthPath string   `json:"healthPath,omitempty"`
	Memory     *Memory  `json:"memory,omitempty"`
}

// Definition is a backend.
type Definition struct {
	Schema        string              `json:"schema"`
	ID            string              `json:"id"`
	Name          string              `json:"name"`
	Engine        string              `json:"engine"`
	Version       string              `json:"version,omitempty"`
	Adapter       string              `json:"adapter"`
	Tasks         []string            `json:"tasks"`
	Capabilities  []string            `json:"capabilities"`
	Formats       []string            `json:"formats"`
	Architectures Architectures       `json:"architectures"`
	Precisions    []string            `json:"precisions,omitempty"` // empty: no restriction declared
	Companions    map[string][]string `json:"companions,omitempty"` // task or capability -> required artifact roles
	Platforms     []Platform          `json:"platforms"`
	Runtime       *Runtime            `json:"runtime,omitempty"`
	Launch        *Launch             `json:"launch,omitempty"`
	Notes         string              `json:"notes,omitempty"`

	BuiltIn bool `json:"-"`
}

var defIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Validate checks a definition.
func (d *Definition) Validate() error {
	switch {
	case d.Schema != Schema:
		return fmt.Errorf("schema must be %q", Schema)
	case !defIDRe.MatchString(d.ID):
		return fmt.Errorf("id %q must be lowercase letters, digits, '.', '_' or '-'", d.ID)
	case strings.TrimSpace(d.Name) == "" || len(d.Name) > 80:
		return errors.New("name is required (at most 80 characters)")
	case !defIDRe.MatchString(d.Engine):
		return errors.New("engine must be a lowercase identifier")
	case !slices.Contains(Adapters, d.Adapter):
		return fmt.Errorf("adapter %q is not an interface this LocalBase implements (%s)", d.Adapter, strings.Join(Adapters, ", "))
	case len(d.Tasks) == 0:
		return errors.New("tasks must list at least one task")
	case len(d.Formats) == 0:
		return errors.New("formats must list at least one format")
	case len(d.Platforms) == 0:
		return errors.New("platforms must list at least one platform")
	}
	for _, t := range d.Tasks {
		if !slices.Contains(models.Tasks, t) {
			return fmt.Errorf("unknown task %q", t)
		}
	}
	for _, p := range d.Platforms {
		if p.OS == "" || p.Arch == "" || p.Accelerator == "" {
			return errors.New("each platform needs os, arch and accelerator")
		}
	}
	if !d.BuiltIn {
		l := d.Launch
		if l == nil || l.Command == "" {
			return errors.New("a custom backend needs launch.command, the absolute path of its executable")
		}
		if !filepath.IsAbs(l.Command) {
			return errors.New("launch.command must be an absolute path")
		}
		if d.Adapter == "process-openai/v1" {
			if l.Memory == nil || l.Memory.WeightsFactor < 1 || l.Memory.OverheadGB < 0 || strings.TrimSpace(l.Memory.Assumption) == "" {
				return errors.New("launch.memory needs weightsFactor >= 1, overheadGb >= 0 and an assumption saying what they cover")
			}
			if !slices.ContainsFunc(l.Args, func(a string) bool { return strings.Contains(a, "{port}") }) {
				return errors.New("launch.args must pass {port} so LocalBase can reach the server")
			}
		}
	}
	return nil
}

// Supports reports task or capability support.
func (d *Definition) Supports(taskOrCapability string) bool {
	return slices.Contains(d.Tasks, taskOrCapability) || slices.Contains(d.Capabilities, taskOrCapability)
}

//go:embed builtin/*.json routing.json
var builtinFS embed.FS

func decode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Registry is the set of backends: built-in plus custom (<root>/backends.d).
type Registry struct {
	mu          sync.RWMutex
	defs        map[string]*Definition
	diagnostics []string // custom definitions that failed to load
	routing     routing
	dir         string
}

// Skipped lists custom definition files that failed to load.
func (r *Registry) Skipped() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.diagnostics...)
}

// Reload re-reads the custom definitions in place.
func (r *Registry) Reload(rootDir string) error {
	fresh, err := Load(rootDir)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.defs, r.diagnostics, r.routing, r.dir = fresh.defs, fresh.diagnostics, fresh.routing, fresh.dir
	r.mu.Unlock()
	return nil
}

// Dir is where custom definitions live.
func Dir(rootDir string) string { return filepath.Join(rootDir, "backends.d") }

// Load reads the built-in definitions and the root's custom ones. A broken
// custom file is reported and skipped; it cannot shadow a built-in id.
func Load(rootDir string) (*Registry, error) {
	r := &Registry{defs: map[string]*Definition{}}
	entries, _ := builtinFS.ReadDir("builtin")
	for _, e := range entries {
		b, _ := builtinFS.ReadFile("builtin/" + e.Name())
		d := &Definition{BuiltIn: true}
		if err := decode(b, d); err != nil {
			return nil, fmt.Errorf("built-in backend %s: %v", e.Name(), err)
		}
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("built-in backend %s: %v", e.Name(), err)
		}
		r.defs[d.ID] = d
	}
	rb, _ := builtinFS.ReadFile("routing.json")
	if err := decode(rb, &r.routing); err != nil || r.routing.Schema != "localbase.routing/v1" {
		return nil, fmt.Errorf("routing table: %v", err)
	}
	if rootDir == "" {
		return r, nil
	}
	r.dir = Dir(rootDir)
	files, _ := filepath.Glob(filepath.Join(r.dir, "*.json"))
	sort.Strings(files)
	for _, f := range files {
		d, err := ReadCustom(f)
		if err == nil && r.defs[d.ID] != nil && r.defs[d.ID].BuiltIn {
			err = fmt.Errorf("id %q is a built-in backend", d.ID)
		}
		if err == nil && filepath.Base(f) != d.ID+".json" {
			err = fmt.Errorf("file name must be %s.json", d.ID)
		}
		if err != nil {
			r.diagnostics = append(r.diagnostics, fmt.Sprintf("%s: %v", filepath.Base(f), err))
			continue
		}
		r.defs[d.ID] = d
	}
	return r, nil
}

// ReadCustom reads and validates a custom definition file.
func ReadCustom(path string) (*Definition, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d := &Definition{}
	if err := decode(b, d); err != nil {
		return nil, fmt.Errorf("invalid backend definition: %v", err)
	}
	return d, d.Validate()
}

// Get returns a definition, or nil.
func (r *Registry) Get(id string) *Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defs[id]
}

// All returns every definition, built-ins first, then by id.
func (r *Registry) All() []*Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Definition, 0, len(r.defs))
	for _, d := range r.defs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BuiltIn != out[j].BuiltIn {
			return out[i].BuiltIn
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Add registers a custom definition by writing <root>/backends.d/<id>.json.
// Registration is the only way a backend joins; nothing a model download
// contains can register one.
func Add(rootDir string, d *Definition) error {
	d.BuiltIn = false
	if err := d.Validate(); err != nil {
		return err
	}
	builtin, err := Load("")
	if err != nil {
		return err
	}
	if builtin.Get(d.ID) != nil {
		return fmt.Errorf("id %q is a built-in backend", d.ID)
	}
	if info, err := os.Stat(d.Launch.Command); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("launch.command %s is not an executable file", d.Launch.Command)
	}
	dir := Dir(rootDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(d, "", "  ")
	tmp := filepath.Join(dir, "."+d.ID+".json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, d.ID+".json"))
}

// Remove unregisters a custom backend. Selections that name it keep the id
// and report it as unavailable.
func Remove(rootDir, id string) (bool, error) {
	if !defIDRe.MatchString(id) {
		return false, fmt.Errorf("invalid backend id %q", id)
	}
	err := os.Remove(filepath.Join(Dir(rootDir), id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
