// Package library joins the model sources (built-in and user catalog,
// imports) with this device's backend selections. Model management, the
// CLI, the admin API, the UI and launch planning all read models through it.
package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"slices"
	"sort"
	"sync"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Library is the model view of one root on this device.
type Library struct {
	Root     *root.Root
	Store    *store.Store
	Catalog  *catalog.Catalog
	Registry *backends.Registry
	host     backends.Host
	DeviceID string

	jobs  jobs
	mu    sync.Mutex
	stamp string
}

// InstallState reports a backend's install state from the managed runtime
// layout (and a custom backend's own executable).
func InstallState(runtimesDir string) func(*backends.Definition) string {
	return func(d *backends.Definition) string {
		if !runsOnThisPlatform(d) {
			return backends.Missing
		}
		if d.Launch != nil && d.Launch.Command != "" {
			if runtime.Executable(d.Launch.Command) {
				return backends.Installed
			}
			return backends.Missing
		}
		if d.Runtime == nil {
			return backends.Missing
		}
		if _, err := runtime.ResolveBinary(runtimesDir, d.Runtime.Executable); err == nil {
			if d.Runtime.Managed || cpuOnly(d) {
				return backends.Installed
			}
			return backends.Missing // a managed package for another build does not run this one
		}
		if (d.Runtime.Managed || cpuOnly(d)) && runtime.HasPinnedRuntime(d.Runtime.Executable) {
			return backends.Installable
		}
		return backends.Missing
	}
}

func cpuOnly(d *backends.Definition) bool {
	for _, p := range d.Platforms {
		if p.Accelerator != "cpu" {
			return false
		}
	}
	return true
}

// Open builds a library for r.
func Open(r *root.Root, st *store.Store, cat *catalog.Catalog, accelerators []string) (*Library, error) {
	reg, err := backends.Load(r.Dir)
	if err != nil {
		return nil, err
	}
	l := &Library{Root: r, Store: st, Catalog: cat, Registry: reg,
		DeviceID: backends.DeviceID(backends.ProbeOptions{})}
	l.host = backends.NewHost(accelerators, reg, InstallState(r.RuntimesDir()))
	l.stamp = "unset"
	l.refresh()
	return l, nil
}

// Entry is a model with where its files live.
type Entry struct {
	Model    models.Model
	Dir      string
	Imported bool
	Spec     *catalog.Spec // nil for imports
}

// ErrNotFound is an unknown model id.
var ErrNotFound = errors.New("model not found")

// Entries lists catalog models, then imports.
func (l *Library) Entries(ctx context.Context) ([]Entry, error) {
	l.refresh()
	var out []Entry
	for _, s := range l.Catalog.Models {
		out = append(out, Entry{Model: models.FromSpec(s), Dir: l.Root.ModelsDir(s.Kind), Spec: s})
	}
	imported, err := l.imported(ctx)
	if err != nil {
		return nil, err
	}
	return append(out, imported...), nil
}

func (l *Library) imported(ctx context.Context) ([]Entry, error) {
	rows, err := l.Store.ImportedModels(ctx)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, row := range rows {
		var m models.Model
		if err := json.Unmarshal(row.Definition, &m); err != nil {
			continue
		}
		out = append(out, Entry{Model: m, Dir: acquire.ImportedDir(l.Root.Dir, m.ID), Imported: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model.ID < out[j].Model.ID })
	return out, nil
}

// Get returns one model.
func (l *Library) Get(ctx context.Context, id string) (*Entry, error) {
	if s := l.Catalog.ByID(id); s != nil {
		return &Entry{Model: models.FromSpec(s), Dir: l.Root.ModelsDir(s.Kind), Spec: s}, nil
	}
	imported, err := l.imported(ctx)
	if err != nil {
		return nil, err
	}
	for i := range imported {
		if imported[i].Model.ID == id {
			return &imported[i], nil
		}
	}
	return nil, ErrNotFound
}

// Inspect reads each variant's files.
func (l *Library) Inspect(e *Entry) map[string]*models.Inspection {
	out := map[string]*models.Inspection{}
	for _, v := range e.Model.Variants {
		in := models.InspectVariant(v, e.Dir)
		out[v.ID] = &in
	}
	return out
}

// Selection resolves the backend that runs a model on this device.
func (l *Library) Selection(ctx context.Context, e *Entry) (backends.Selection, error) {
	l.refresh()
	c, err := l.Store.BackendChoice(ctx, e.Model.ID, l.DeviceID)
	if err != nil {
		return backends.Selection{}, err
	}
	var choice *backends.Choice
	if c != nil {
		choice = &backends.Choice{Mode: backends.ModeManual, BackendID: c.BackendID, VariantID: c.VariantID}
	}
	return l.Registry.Resolve(&e.Model, l.Host(), l.Inspect(e), choice), nil
}

// SetBackend stores a manual choice. Any registered backend is accepted,
// whatever detection says; the finding is returned for display.
func (l *Library) SetBackend(ctx context.Context, id, backendID, variantID string) (backends.Selection, error) {
	l.refresh()
	e, err := l.Get(ctx, id)
	if err != nil {
		return backends.Selection{}, err
	}
	if l.Registry.Get(backendID) == nil {
		return backends.Selection{}, fmt.Errorf("backend %q is not registered", backendID)
	}
	if variantID != "" && e.Model.Variant(variantID) == nil {
		return backends.Selection{}, fmt.Errorf("model %s has no variant %q", id, variantID)
	}
	if err := l.Store.SetBackendChoice(ctx, id, l.DeviceID, backendID, variantID); err != nil {
		return backends.Selection{}, err
	}
	return l.Selection(ctx, e)
}

// ResetBackend returns a model to automatic selection on this device.
func (l *Library) ResetBackend(ctx context.Context, id string) (backends.Selection, error) {
	e, err := l.Get(ctx, id)
	if err != nil {
		return backends.Selection{}, err
	}
	if _, err := l.Store.ResetBackendChoice(ctx, id, l.DeviceID); err != nil {
		return backends.Selection{}, err
	}
	return l.Selection(ctx, e)
}

// SaveImport stores an imported model and, when the person chose a backend
// at import time, that manual choice.
func (l *Library) SaveImport(ctx context.Context, res *acquire.Result, backendID string) (backends.Selection, error) {
	b, err := json.Marshal(res.Model)
	if err != nil {
		return backends.Selection{}, err
	}
	if err := l.Store.SaveImportedModel(ctx, res.Model.ID, b); err != nil {
		return backends.Selection{}, err
	}
	if backendID != "" && backendID != "auto" {
		return l.SetBackend(ctx, res.Model.ID, backendID, "")
	}
	e, err := l.Get(ctx, res.Model.ID)
	if err != nil {
		return backends.Selection{}, err
	}
	return l.Selection(ctx, e)
}

var llmTasks = []string{"chat", "embeddings", "decision"}

// Gate resolves a model for the LLM runtime through the selection.
func (l *Library) Gate() runtime.Gate {
	return func(modelID string) (*runtime.Target, error) {
		ctx := context.Background()
		e, err := l.Get(ctx, modelID)
		if errors.Is(err, ErrNotFound) {
			return nil, runtime.ErrModelNotFound
		}
		if err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(llmTasks, e.Model.HasTask) {
			return nil, runtime.ErrModelNotFound
		}
		s, err := l.Selection(ctx, e)
		if err != nil {
			return nil, err
		}
		t := &runtime.Target{ModelID: modelID, Model: &e.Model, Dir: e.Dir, Manual: s.Mode == backends.ModeManual, Imported: e.Imported}
		switch {
		case s.Unavailable:
			return t, &runtime.LaunchError{Code: "backend_unavailable", Message: fmt.Sprintf(
				"The backend chosen for %s (%s) is no longer registered. Choose another backend for this model.", modelID, s.BackendID)}
		case s.BackendID == "":
			return t, &runtime.LaunchError{Code: "backend_not_selected", Message: fmt.Sprintf(
				"No backend is selected for %s on this device. Select a backend to run this model.", modelID)}
		}
		t.Backend = l.Registry.Get(s.BackendID)
		t.Variant = e.Model.Variant(s.VariantID)
		if t.Variant == nil {
			return t, &runtime.LaunchError{Code: "backend_not_selected", Message: "The selected variant no longer exists. Select a backend to run this model."}
		}
		if e.Imported && len(s.NeedsDownload) > 0 {
			return t, &runtime.LaunchError{Code: "model_assets_missing", Message: fmt.Sprintf(
				"%s is missing files: %v. Import them again or choose another variant.", modelID, s.NeedsDownload)}
		}
		return t, nil
	}
}

func runsOnThisPlatform(d *backends.Definition) bool {
	for _, p := range d.Platforms {
		if (p.OS == goruntime.GOOS || p.OS == "*") && (p.Arch == backends.GoArch() || p.Arch == "*") {
			return true
		}
	}
	return false
}

// Preview rates a resolved import before downloading, from the facts the
// source publishes. The recommendation is checked again against the
// downloaded files.
func (l *Library) Preview(p *acquire.Plan) backends.Recommendation {
	l.refresh()
	m := models.Model{ID: p.ID, Name: p.Name, Tasks: []string{p.Task}, Capabilities: []string{}, Variants: []models.Variant{p.Preview}}
	if p.Task == "" {
		m.Tasks = nil
	}
	if p.Task == "chat" && slices.ContainsFunc(p.Files, func(f acquire.PlannedFile) bool { return f.Role == models.RoleProjector }) {
		m.Capabilities = []string{"vision"}
	}
	return l.Registry.Recommend(&m, l.Host(), nil)
}

// refresh reloads the registry when backends.d changed (a backend added or
// removed by the CLI or the UI while the gateway runs).
func (l *Library) refresh() {
	stamp := ""
	if entries, err := os.ReadDir(backends.Dir(l.Root.Dir)); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				stamp += e.Name() + info.ModTime().String() + ";"
			}
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if stamp == l.stamp {
		return
	}
	if l.Registry.Reload(l.Root.Dir) != nil {
		return
	}
	l.stamp = stamp
	l.host = backends.NewHost(l.host.Accelerators, l.Registry, InstallState(l.Root.RuntimesDir()))
}

// Host is this device as detection sees it.
func (l *Library) Host() backends.Host {
	l.refresh()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.host
}

// SelectionKey identifies a model's current selection: the stored manual
// choice (or automatic mode) and the registry state. It is one primary-key
// read, cheap enough for every request.
func (l *Library) SelectionKey(modelID string) string {
	l.refresh()
	l.mu.Lock()
	stamp := l.stamp
	l.mu.Unlock()
	c, err := l.Store.BackendChoice(context.Background(), modelID, l.DeviceID)
	switch {
	case err != nil:
		return "error"
	case c == nil:
		return "auto|" + stamp
	}
	return "manual|" + c.BackendID + "|" + c.VariantID + "|" + stamp
}
