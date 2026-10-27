package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// Row is one model in the library.
type Row struct {
	ID, Name, Initial, Task, Provider string
	IsChat                            bool
	DiskBytes                         int64
	MemoryGB                          int // computed minimum, 0 = unknown
	MemorySource                      string
	Enabled, Default, Loaded, Busy    bool
	Trust                             string
}

func task(s *catalog.Spec) (string, bool) {
	switch s.Kind {
	case "llm":
		switch {
		case s.IsEmbedding():
			return "Embeddings", false
		case s.IsDecision():
			return "Decisions", false
		}
		return "Chat", true
	case "stt":
		return "Transcription", false
	case "tts":
		return "Speech", false
	case "image":
		return "Images", false
	}
	return "Video", false
}

// installedBytes returns the bytes on disk when every artifact is present at
// its expected size, else 0. It never hashes files.
func (u *UI) installedBytes(s *catalog.Spec) int64 {
	dir := filepath.Join(u.d.RootDir, "models", s.Kind)
	var total int64
	for _, a := range s.Artifacts {
		info, err := os.Stat(filepath.Join(dir, a.Filename))
		if err != nil || !info.Mode().IsRegular() || (a.ExpectedSizeBytes > 0 && info.Size() != a.ExpectedSizeBytes) {
			return 0
		}
		total += info.Size()
	}
	return total
}

func (u *UI) rows() []Row {
	cfg := u.d.LLM.Config()
	runningID, _, active := u.d.LLM.Running()
	var rows []Row
	for _, s := range u.d.Catalog.All() {
		disk := u.installedBytes(s)
		if disk == 0 {
			continue
		}
		t, chat := task(s)
		r := Row{ID: s.ModelID, Name: s.Name(), Initial: initial(s.Name()), Task: t, IsChat: chat,
			Provider: s.Provider, DiskBytes: disk, Trust: s.Trust,
			Enabled: slices.Contains(cfg.Selected[s.Kind], s.ModelID) || cfg.Active[s.Kind] == s.ModelID,
			Default: cfg.Active[s.Kind] == s.ModelID, Loaded: runningID == s.ModelID}
		r.Busy = r.Loaded && active > 0
		if f := runtime.ModelFit(context.Background(), u.d.Store, u.d.Catalog, s, filepath.Join(u.d.RootDir, "models", s.Kind), u.memoryGB()); f != nil {
			r.MemoryGB, r.MemorySource = f.MinVramGB, f.Source
		}
		rows = append(rows, r)
	}
	taskOrder := map[string]int{"Chat": 0, "Decisions": 1, "Embeddings": 2, "Transcription": 3, "Speech": 4, "Images": 5, "Video": 6}
	slices.SortStableFunc(rows, func(a, b Row) int {
		key := func(r Row) [3]int {
			return [3]int{taskOrder[r.Task], btoi(!r.Default), btoi(!r.Enabled)}
		}
		ka, kb := key(a), key(b)
		for i := range ka {
			if ka[i] != kb[i] {
				return ka[i] - kb[i]
			}
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return rows
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (u *UI) memoryGB() int {
	if u.d.Memory == nil {
		return 0
	}
	return int(float64(u.d.Memory.Topology().SystemCapacity)/float64(memory.GiB) + 0.5)
}

func freeDisk(dir string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

// taskOrder lists tasks the way the Library and Get models show them.
var taskOrder = []string{"Chat", "Images", "Speech", "Transcription", "Video", "Embeddings", "Decisions"}

// taskFilter reads ?filter: "all", a task in lower case, or the older
// "other" (every task but chat).
func taskFilter(r *http.Request) string {
	f := r.URL.Query().Get("filter")
	if f == "other" || slices.ContainsFunc(taskOrder, func(t string) bool { return strings.ToLower(t) == f }) {
		return f
	}
	return "all"
}

func matchesTask(filter, task string) bool {
	return filter == "all" || (filter == "other" && task != "Chat") || strings.ToLower(task) == filter
}

// presentTasks are the filter chips: the tasks that have at least one model.
func presentTasks(tasks []string) []string {
	var out []string
	for _, t := range taskOrder {
		if slices.Contains(tasks, t) {
			out = append(out, t)
		}
	}
	return out
}

func (u *UI) library(w http.ResponseWriter, r *http.Request) {
	filter := taskFilter(r)
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	all := u.rows()
	var shown []Row
	var disk int64
	var tasks []string
	for _, row := range all {
		disk += row.DiskBytes
		tasks = append(tasks, row.Task)
		if !matchesTask(filter, row.Task) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(row.Name+" "+row.Task+" "+row.Provider+" "+row.ID), strings.ToLower(q)) {
			continue
		}
		shown = append(shown, row)
	}
	u.render(w, r, "library", map[string]any{
		"Rows": shown, "Installed": len(all), "Filter": filter, "Query": q, "DiskBytes": disk,
		"FreeDisk": freeDisk(u.d.RootDir), "Tasks": presentTasks(tasks),
	})
}

// CatalogRow is a qualified catalog model on Get models.
type CatalogRow struct {
	ID, Name, Initial, Task string
	DownloadBytes           int64
	Fit                     string // fits | tight | wont-fit | "" (unknown)
	Installed               bool
}

// catalogRows are the qualified models LocalBase can install here, by task,
// those that fit first. Experimental runtimes stay out of the list.
func (u *UI) catalogRows(filter string) (rows []CatalogRow, tasks []string) {
	for _, s := range u.d.Catalog.All() {
		if !s.Qualified() || s.ExperimentalRuntime {
			continue
		}
		t, _ := task(s)
		tasks = append(tasks, t)
		if !matchesTask(filter, t) {
			continue
		}
		row := CatalogRow{ID: s.ModelID, Name: s.Name(), Initial: initial(s.Name()), Task: t, Installed: u.installedBytes(s) > 0}
		for _, a := range s.Artifacts {
			row.DownloadBytes += a.ExpectedSizeBytes
		}
		if f := runtime.ModelFit(context.Background(), u.d.Store, u.d.Catalog, s, filepath.Join(u.d.RootDir, "models", s.Kind), u.memoryGB()); f != nil {
			row.Fit = f.Fit
		}
		rows = append(rows, row)
	}
	fitRank := map[string]int{"fits": 0, "tight": 1, "": 2, "wont-fit": 3}
	slices.SortStableFunc(rows, func(a, b CatalogRow) int {
		if d := slices.Index(taskOrder, a.Task) - slices.Index(taskOrder, b.Task); d != 0 {
			return d
		}
		if d := fitRank[a.Fit] - fitRank[b.Fit]; d != 0 {
			return d
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return rows, presentTasks(tasks)
}

func (u *UI) running(w http.ResponseWriter, r *http.Request) {
	snap := u.d.Memory.Snapshot(r.Context())
	capacity := u.d.Memory.Topology().SystemCapacity
	id, plan, active := u.d.LLM.Running()
	var models int64
	var item map[string]any
	if id != "" {
		used, measured := u.d.LLM.Footprint(r.Context())
		if !measured {
			used = plan.Demand.UnifiedBytes
		}
		models = used
		name := id
		if s := u.d.Catalog.ByID(id); s != nil {
			name = s.Name()
		}
		item = map[string]any{"ID": id, "Name": name, "Bytes": used, "Measured": measured, "Active": active,
			"Slots": plan.Parallel.Slots, "Context": plan.Budget.EffectiveContext}
	}
	other := capacity - max(snap.Available, 0) - models
	u.render(w, r, "running", map[string]any{
		"Capacity": capacity, "Available": snap.Available, "Models": models, "Other": max(other, 0),
		"Item": item,
	})
}

func (u *UI) details(w http.ResponseWriter, r *http.Request) {
	s := u.d.Catalog.ByID(r.PathValue("id"))
	var backend any
	if u.d.Library != nil {
		if e, err := u.d.Library.Get(r.Context(), r.PathValue("id")); err == nil {
			if sel, err := u.d.Library.Selection(r.Context(), e); err == nil {
				backend = u.backendView(e.Model.ID, sel)
			}
			if s == nil && e.Imported {
				ins := u.d.Library.Inspect(e)
				u.render(w, r, "details", map[string]any{"Imported": e, "Inspection": ins, "Backend": backend})
				return
			}
		}
	}
	if s == nil {
		http.NotFound(w, r)
		return
	}
	t, _ := task(s)
	var geo *memory.Geometry
	if s.Kind == "llm" {
		geo = memory.ReadGeometry(filepath.Join(u.d.RootDir, "models", "llm", s.Primary().Filename))
	}
	var peaks []store.ObservedPeak
	if sha := s.Primary().SHA256; sha != "" {
		peaks, _ = u.d.Store.ObservedPeaks(r.Context(), s.ModelID, sha)
	}
	var place *placementView
	if s.Kind == "llm" {
		place = u.placementData(r, s.ModelID, "", "")
	}
	u.render(w, r, "details", map[string]any{"Spec": s, "Task": t, "Disk": u.installedBytes(s),
		"Fit": runtime.FitFor(s, u.memoryGB(), geo, peaks, u.d.Catalog.PlannableFeedPeaks(s)), "Backend": backend, "Placement": place})
}
