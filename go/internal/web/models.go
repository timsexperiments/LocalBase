package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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

func (u *UI) rows() ([]Row, bool) {
	missing := false
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
		f, err := runtime.ModelFit(context.Background(), u.d.Store, u.d.Catalog, s, filepath.Join(u.d.RootDir, "models", s.Kind), u.memoryGB())
		missing = missing || err != nil
		if f != nil {
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
	return rows, missing
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
	all, missing := u.rows()
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
	readNotice := ""
	if missing {
		readNotice = missingFiguresNotice
	}
	u.render(w, r, "library", map[string]any{"ReadNotice": readNotice,
		"Rows": shown, "Installed": len(all), "Filter": filter, "Query": q, "DiskBytes": disk,
		"FreeDisk": freeDisk(u.d.RootDir), "Tasks": presentTasks(tasks),
	})
}

// CatalogRow is a qualified catalog model on Get models.
type CatalogRow struct {
	ID, Name, Initial, Task string
	DownloadBytes           int64
	UsesBytes               int64  // memory while running, at these launch settings
	FreeFirstBytes          int64  // how much to free before it would start, when it wouldn't now
	Fit                     string // fits | tight | close-others | wont-fit | "" (unknown)
	Forecast                bool   // Fit is the gateway's answer for right now, not total memory
	MayNotFit               string // the catalog's rough figure says no: words, and the choice stays open
	Installed               bool
	EditsPictures           bool // the catalog says it can start from a picture (image-to-image)
}

// catalogRows are the qualified models LocalBase can install here, by task,
// those that fit first. Experimental runtimes stay out of the list.
func (u *UI) catalogRows(filter string) (rows []CatalogRow, tasks []string, missing bool) {
	for _, s := range u.d.Catalog.All() {
		if !s.Qualified() || s.ExperimentalRuntime {
			continue
		}
		t, _ := task(s)
		tasks = append(tasks, t)
		if !matchesTask(filter, t) {
			continue
		}
		row := CatalogRow{ID: s.ModelID, Name: s.Name(), Initial: initial(s.Name()), Task: t, Installed: u.installedBytes(s) > 0, EditsPictures: s.HasCapability("image-to-image")}
		for _, a := range s.Artifacts {
			row.DownloadBytes += a.ExpectedSizeBytes
		}
		f, err := runtime.ModelFit(context.Background(), u.d.Store, u.d.Catalog, s, filepath.Join(u.d.RootDir, "models", s.Kind), u.memoryGB())
		missing = missing || err != nil
		if f != nil {
			row.Fit, row.UsesBytes = f.Fit, f.DemandBytes
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
	return rows, presentTasks(tasks), missing
}

func (u *UI) running(w http.ResponseWriter, r *http.Request) {
	u.renderRunning(w, r, "", false)
}

// runningRow is one runtime holding memory, as the legend counts it.
type runningRow struct {
	Runtime, ID, Name, Task string
	Bytes                   int64
	Source                  string // measured | estimated
	Active, Waiting         int
	State                   string
	Sheet                   string
}

var runtimeTasks = map[string]string{"llm": "Chat", "stt": "Transcription", "tts": "Speech", "image": "Images", "video": "Video"}

// runningRows is one row for every runtime in the host reading, so the screen agrees with its
// own legend. The text model also gets a row if the reading hasn't caught up with it yet.
func (u *UI) runningRows(h *hostReading, llmID string, llmPlanned int64, llmActive int, st runtime.Snapshot) []runningRow {
	rows := []runningRow{}
	seenLLM := false
	add := func(rt, id string, bytes int64, source string, a hostActivity) {
		row := runningRow{Runtime: rt, ID: id, Name: id, Task: runtimeTasks[rt], Bytes: bytes, Source: rowSource[source], Active: a.InFlight, Waiting: a.Queued, Sheet: "unload-sheet-" + rt}
		if s := u.d.Catalog.ByID(id); s != nil {
			row.Name = s.Name()
			if t, _ := task(s); t != "" {
				row.Task = t
			}
		}
		if rt == "llm" && (llmID == "" || id == llmID) {
			row.State, row.Active, row.Waiting, row.Sheet = st.State, llmActive, st.Waiting, "unload-sheet"
		}
		// Two models can share a runtime; each still needs its own sheet.
		if slices.ContainsFunc(rows, func(o runningRow) bool { return o.Sheet == row.Sheet }) {
			row.Sheet = fmt.Sprintf("%s-%d", row.Sheet, len(rows))
		}
		rows = append(rows, row)
	}
	for _, rt := range h.Host.Memory.LocalBase.Runtimes {
		// A reservation with no model yet is a model on its way, which the page says apart.
		if rt.ModelID == nil || *rt.ModelID == "" {
			continue
		}
		seenLLM = seenLLM || rt.Runtime == "llm"
		add(rt.Runtime, *rt.ModelID, rt.Bytes, rt.Source, h.activity(rt.Runtime))
	}
	if llmID != "" && !seenLLM {
		add("llm", llmID, llmPlanned, "planned", hostActivity{})
	}
	return rows
}

// renderRunning draws Running now; notice is the outcome of an unload.
func (u *UI) renderRunning(w http.ResponseWriter, r *http.Request, notice string, failed bool) {
	id, plan, active := u.d.LLM.Running()
	h, ok := u.readHost(r)
	if !ok {
		h = &hostReading{}
	}
	state := u.d.LLM.Snapshot()
	var planned int64
	if id != "" {
		planned = plan.Demand.UnifiedBytes
	}
	rows := u.runningRows(h, id, planned, active, state)
	// Whatever the rows don't name: a model on its way holds memory before it has a row.
	listed := int64(0)
	for _, row := range rows {
		listed += row.Bytes
	}
	gettingReady := h.Host.Memory.LocalBase.Bytes - listed
	// One sentence for screen readers, announced only when it changes.
	summary := "Nothing is loaded."
	switch {
	case state.State == "starting":
		summary = "A model is starting."
	case len(rows) > 1:
		summary = fmt.Sprintf("%d models are loaded.", len(rows))
	case len(rows) == 1 && rows[0].State == "draining":
		summary = fmt.Sprintf("%s is finishing before another model loads.", rows[0].Name)
	case len(rows) == 1 && rows[0].Active > 0:
		summary = fmt.Sprintf("%s is busy.", rows[0].Name)
	case len(rows) == 1:
		summary = fmt.Sprintf("%s is loaded and idle.", rows[0].Name)
	}
	data := u.memoryView(r, h)
	for k, v := range map[string]any{"GettingReady": max(gettingReady, 0), "BusyWith": h.busyWith(), "Rows": rows, "Starting": state.State == "starting" && len(rows) == 0, "Summary": summary,
		"CanManage": u.caller(r).Can("models:manage"), "Notice": notice, "Failed": failed} {
		data[k] = v
	}
	u.render(w, r, "running", data)
}

// unload asks the gateway to unload a model, waiting up to 30 s for its
// requests to finish (DESIGN §23), then redraws the page with the outcome.
func (u *UI) unload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res := u.call(r, "POST", "/_localbase/models/"+url.PathEscape(id)+"/unload", "", nil)
	var out struct {
		Freed int64 `json:"memoryFreedBytes"`
		Error struct {
			Code     string `json:"code"`
			Message  string `json:"message"`
			InFlight int    `json:"in_flight"`
		} `json:"error"`
	}
	json.Unmarshal(res.Body, &out)
	switch {
	case res.Status == http.StatusOK && out.Freed > 0:
		u.renderRunning(w, r, fmt.Sprintf("Freed %.1f GB. Its files stay installed.", float64(out.Freed)/1e9), false)
	case res.Status == http.StatusOK:
		u.renderRunning(w, r, "Done. Its files stay installed.", false)
	case out.Error.Code == "model_busy" && out.Error.InFlight > 0:
		u.renderRunning(w, r, fmt.Sprintf("It’s busy with %d %s. Try again when they finish.", out.Error.InFlight, map[bool]string{true: "request", false: "requests"}[out.Error.InFlight == 1]), true)
	case out.Error.Code == "model_busy":
		u.renderRunning(w, r, "It is busy, so it stayed loaded. Try again in a moment.", true)
	case out.Error.Code == "model_not_loaded":
		u.renderRunning(w, r, "It was already out of memory.", false)
	case res.Status == http.StatusNotFound && out.Error.Code == "":
		u.renderRunning(w, r, "This LocalBase can’t free a model’s memory yet.", true)
	default:
		msg := out.Error.Message
		if msg == "" {
			msg = "Its memory couldn’t be freed. Try again."
		}
		u.renderRunning(w, r, msg, true)
	}
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
	readNotice := ""
	if sha := s.Primary().SHA256; sha != "" {
		var err error
		if peaks, err = u.d.Store.ObservedPeaks(r.Context(), s.ModelID, sha); err != nil {
			readNotice = "What this model was seen to need couldn't be read, so this is its estimate. Try again in a moment."
		}
	}
	var place *placementView
	if s.Kind == "llm" {
		place = u.placementData(r, s.ModelID, "", "")
	}
	u.render(w, r, "details", map[string]any{"ReadNotice": readNotice, "Spec": s, "Task": t, "Disk": u.installedBytes(s),
		"Fit": runtime.FitFor(s, u.memoryGB(), geo, peaks, u.d.Catalog.PlannableFeedPeaks(s)), "Backend": backend, "Placement": place})
}

// firstWords names something by how it begins.
func firstWords(text string) string {
	words := strings.Fields(text)
	if len(words) <= 6 {
		return strings.TrimRight(strings.Join(words, " "), " .,;:!?")
	}
	return strings.TrimRight(strings.Join(words[:6], " "), " .,;:!?") + "…"
}

// missingFiguresNotice is what a list says when it couldn't read some memory figures.
const missingFiguresNotice = "Some memory figures couldn't be read, so they're left out. Try again in a moment."
