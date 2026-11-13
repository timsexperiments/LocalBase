package main

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/backup"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/meta"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/safefile"
	"github.com/timsexperiments/LocalBase/go/internal/service"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
	"github.com/timsexperiments/LocalBase/go/internal/version"
)

// `diagnostics`: a redacted ZIP for bug reports (TS diagnostics.ts). The
// manifest holds hardware, configuration shape, models, service, telemetry,
// log and voice summaries; logs/events.jsonl holds the newest events,
// redacted again. Each section is reported unavailable rather than failing
// the bundle. Nothing is created or migrated: the database is read-only.

const (
	maxDiagnosticEvents  = 500
	maxDiagnosticLogs    = 2 << 20
	maxDiagnosticsSource = 5 << 20
	maxDiagnosticsZip    = 5 << 20
)

func diagnosticsCommand() *cli.Command {
	return &cli.Command{Name: "diagnostics", Summary: "Create a redacted LocalBase diagnostics bundle",
		Examples: []string{"local-base diagnostics", "local-base diagnostics --output report.zip"},
		Options:  []cli.Option{{Name: "output", Value: "<path.zip>", Help: "Output ZIP path"}},
		Run:      runDiagnostics}
}

// object builds a jsjson object from key/value pairs.
func object(kv ...any) *jsjson.Object {
	o := jsjson.NewObject()
	for i := 0; i < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func available(data any) *jsjson.Object { return object("available", true, "data", data) }
func unavailable(code string) *jsjson.Object {
	return object("available", false, "error", code)
}

// databaseProblem is the manifest's code for a database that couldn't be read. An unfinished write says
// more, and what to do about it, so it wins.
func databaseProblem(openFailed, unfinishedWrite bool) string {
	switch {
	case unfinishedWrite:
		return "unfinished_write"
	case openFailed:
		return "database_unavailable"
	}
	return ""
}

// nodeArch is process.arch.
func nodeArch() string {
	switch goruntime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	}
	return goruntime.GOARCH
}

func strs(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

func nullable[T any](p *T) any {
	if p == nil {
		return nil
	}
	return jsjson.FromGo(*p)
}

// readProblems are the reads a bundle had to go without: the step and the reason, with no paths.
type readProblems struct {
	dir  string
	list []any
	text []string
}

// addReason records a step with a reason in words of ours, for an error whose text mustn't leave stderr.
func (p *readProblems) addReason(step, reason string) {
	p.list = append(p.list, object("step", step, "reason", reason))
	p.text = append(p.text, step+": "+reason)
}

func (p *readProblems) add(step string, err error) {
	reason := "the database is busy"
	if !store.IsBusy(err) {
		reason = strings.ReplaceAll(err.Error(), p.dir, "<root>")
	}
	p.list = append(p.list, object("step", step, "reason", reason))
	p.text = append(p.text, step+": "+reason)
}

func runDiagnostics(c *cli.Ctx) (any, error) {
	requested := c.String("output")
	if c.Has("output") && (requested == "" || len(requested) > 4096) {
		return nil, cli.Inputf("output: Too small: expected string to have >=1 characters")
	}
	dir, err := root.Canonical(root.Resolve(c.Global.Root))
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	generatedAt := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	specs := memory.DetectSpecs()
	systemGB := specs.GPUVramGB
	hardware := available(object("os", specs.OSName, "arch", nodeArch(), "cpu", specs.CPUModel, "gpu", specs.GPUName,
		"memoryGb", float64(specs.RAMGB), "vramGb", float64(specs.GPUVramGB)))

	// The bundle is how a damaged root gets reported, so a read that fails doesn't stop it. It says so.
	problems := &readProblems{dir: dir}
	// The database, read-only: no section creates or migrates it.
	var st *store.Store
	var databaseErr error
	if _, err := os.Stat(filepath.Join(dir, "local-base.db")); err == nil {
		if st, err = store.OpenReadOnly(filepath.Join(dir, "local-base.db")); err != nil {
			st, databaseErr = nil, err
			// The step and a word for why; the error itself goes to stderr only, since a bundle gets
			// shared and the error can hold a path.
			problems.addReason("opening the database", (&failedRead{Step: "opening the database", Err: err}).Reason())
		}
	}
	if st != nil {
		defer st.Close()
	}
	var cfg *store.Config
	cat, catErr := catalog.LoadRoot(dir, nil)
	configuration := unavailable("configuration_unavailable")
	if st != nil && catErr == nil {
		loaded, err := st.LoadConfig(ctx)
		if err != nil && !errors.Is(err, store.ErrNotConfigured) {
			problems.add("reading the saved settings", err)
		}
		if err == nil {
			cfg = loaded
			var parallel any = cfg.Parallel
			if n, err := strconv.Atoi(cfg.Parallel); err == nil {
				parallel = float64(n)
			}
			selected, active := jsjson.NewObject(), jsjson.NewObject()
			for _, k := range store.Kinds {
				selected.Set(k, strs(cfg.Selected[k]))
				active.Set(k, cfg.Active[k])
			}
			configuration = available(object("contextSize", float64(cfg.CtxSize), "contextMode", cfg.CtxMode, "parallel", parallel,
				"selectedModels", selected, "activeModels", active))
		}
	}

	models := unavailable("models_unavailable")
	var modelData *jsjson.Object
	if cfg != nil {
		modelData = diagnosticModels(ctx, dir, st, cat, cfg, systemGB, problems)
		models = available(modelData)
	}

	service := unavailable("service_unavailable")
	if in, err := serviceInspection(dir); err == nil {
		gw := object("state", in.Gateway.State)
		if len(in.Gateway.Health) > 0 {
			h, _ := jsjson.Parse(in.Gateway.Health)
			gw.Set("health", h)
		}
		var managerState any
		if in.Service.ManagerState != nil && diagnosticManagerState(*in.Service.ManagerState) {
			managerState = *in.Service.ManagerState
		}
		service = available(object("manager", in.Service.Manager, "state", in.Service.State,
			"enabled", nullable(in.Service.Enabled), "definitionInstalled", in.Service.DefinitionInstalled,
			"managerAvailable", in.Service.ManagerAvailable, "managerState", managerState,
			"pid", nullable(in.Service.PID), "uptimeSeconds", nullable(in.Service.UptimeSeconds),
			"restartCount", nullable(in.Service.RestartCount), "gateway", gw))
	}

	observability := unavailable("otel_unavailable")
	if cfg != nil {
		if o, err := obs.ResolveOtel(cfg.OtelEndpoint, cfg.OtelHeaders, cfg.OtelSampleRatio, os.Getenv); err == nil {
			source, endpoint := "disabled", ""
			if o.Enabled {
				source, endpoint = o.Source, o.DisplayEndpoint
			}
			observability = available(object("enabled", o.Enabled, "source", source, "endpoint", endpoint, "sampleRatio", o.SampleRatio))
		}
	}

	stderr := obs.InspectServiceStderr(dir)
	logs := unavailable("logs_unavailable")
	var logText bytes.Buffer
	if _, err := root.Open(dir); err == nil {
		if events, err := obs.Snapshot(dir, obs.Filters{}, maxDiagnosticEvents); err == nil {
			if chosen, size, err := boundedDiagnosticLogs(events); err == nil {
				window := jsjson.NewObject()
				if len(chosen) > 0 {
					oldest, _ := chosen[0].Get("timestamp")
					newest, _ := chosen[len(chosen)-1].Get("timestamp")
					window.Set("oldest", oldest)
					window.Set("newest", newest)
				}
				data := object("includedEvents", float64(len(chosen)), "window", window, "bytes", float64(size))
				if stderr != nil {
					data.Set("serviceStderr", object("bytes", float64(stderr.Bytes), "archiveBytes", float64(stderr.ArchiveBytes),
						"modifiedAt", stderr.ModifiedAt))
				}
				logs = available(data)
				for _, e := range chosen {
					logText.Write(jsjson.Stringify(e))
					logText.WriteByte('\n')
				}
			}
		}
	}

	voices := unavailable("voices_unavailable")
	if st != nil {
		if n, err := st.CountVoices(ctx); err == nil {
			voices = available(object("customVoices", float64(n)))
		}
	}

	manifest := object("schemaVersion", float64(1), "generatedAt", generatedAt, "version", version.Version,
		"platform", object("os", goruntime.GOOS, "arch", nodeArch()), "hardware", hardware, "configuration", configuration,
		"models", models, "service", service, "observability", observability, "logs", logs, "voices", voices)
	// A bundle is how a damaged root gets reported, so it is still made, and says so: what it
	// could not read from the database is marked unavailable above.
	// The bundle says only that it couldn't: bundles get shared, and the error can hold a path.
	if databaseErr != nil {
		c.Error("Could not read this root's database: %s", termsafe.Printable(databaseErr.Error()))
	}
	unfinished := backup.UnfinishedWrite(ctx, dir)
	if code := databaseProblem(databaseErr != nil, unfinished != ""); code != "" {
		manifest.Set("database", unavailable(code))
	}
	if len(problems.list) > 0 {
		manifest.Set("readProblems", problems.list)
	}
	entries := []zipEntry{
		{"manifest.json", append(jsjson.Indent(manifest), '\n')},
		{"logs/events.jsonl", logText.Bytes()},
	}
	path, err := diagnosticsOutput(requested)
	if err != nil {
		return nil, err
	}
	size, err := writeDiagnosticsArchive(path, entries)
	if err != nil {
		return nil, err
	}
	c.Info("Created diagnostics bundle: %s", path)
	for _, line := range problems.text {
		c.Info("Could not read: %s", line)
	}
	if unfinished != "" {
		c.Info("%s", unfinished)
	}
	if stderr != nil {
		c.Info("Service stderr: %d bytes captured at %s. It holds unredacted gateway crash output, so the bundle records only its size; review it before sharing.",
			stderr.Bytes, stderr.Path)
	}
	if modelData != nil {
		printDiagnosticModels(c, modelData)
	}
	return map[string]any{"archive": map[string]any{"path": path, "entries": len(entries), "bytes": size}}, nil
}

// serviceInspection reads the service state without changing anything.
func serviceInspection(dir string) (service.Inspection, error) {
	return service.Inspect(&root.Root{Dir: dir}, false)
}

func diagnosticManagerState(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._/-", r)) {
			return false
		}
	}
	return true
}

// boundedDiagnosticLogs keeps the newest events that fit the event and
// byte limits, oldest first.
func boundedDiagnosticLogs(events []*obs.Event) ([]*jsjson.Object, int, error) {
	var chosen []*jsjson.Object
	size := 0
	for i := len(events) - 1; i >= 0; i-- {
		e, err := obs.DiagnosticsEvent(events[i].Raw)
		if err != nil {
			return nil, 0, err
		}
		n := len(jsjson.Stringify(e)) + 1
		if len(chosen) >= maxDiagnosticEvents || size+n > maxDiagnosticLogs {
			break
		}
		chosen = append([]*jsjson.Object{e}, chosen...)
		size += n
	}
	return chosen, size, nil
}

// diagnosticModels lists selected or installed catalog models with their
// provenance, capabilities and memory figure, plus catalog problems and
// the feed's state.
func diagnosticModels(ctx context.Context, dir string, st *store.Store, cat *catalog.Catalog, cfg *store.Config, systemGB int, problems *readProblems) *jsjson.Object {
	selected, active := map[string]bool{}, map[string]bool{}
	for _, k := range store.Kinds {
		for _, id := range cfg.Selected[k] {
			selected[id] = true
		}
		active[cfg.Active[k]] = true
	}
	list := []any{}
	for _, s := range cat.All() {
		modelDir := filepath.Join(dir, "models", s.Kind)
		complete, _ := s.Installation(modelDir)
		if !selected[s.ModelID] && !complete {
			continue
		}
		var observed []store.ObservedPeak
		if sha := s.Primary().SHA256; st != nil && sha != "" {
			var err error
			if observed, err = st.ObservedPeaks(ctx, s.ModelID, sha); err != nil {
				problems.add("reading the memory peaks seen for "+s.ModelID, err)
			}
		}
		fit := runtime.FitFor(s, systemGB, nil, observed, cat.PlannableFeedPeaks(s))
		report := runtime.CapabilitiesFor(ctx, st, cat, s, modelDir)
		caps := []any{}
		for _, cp := range report.Capabilities {
			caps = append(caps, object("id", cp.ID, "source", cp.Source))
		}
		mismatches := []any{}
		for _, m := range report.Mismatches {
			mismatches = append(mismatches, jsjson.FromGo(m))
		}
		unsupported := []any{}
		for _, u := range s.UnsupportedCapabilities {
			unsupported = append(unsupported, jsjson.FromGo(u))
		}
		var build, peak any
		if report.RuntimeBuild != "" {
			build = report.RuntimeBuild
		}
		if fit.Observed != nil {
			peak = float64(fit.Observed.PeakBytes)
		}
		list = append(list, object("id", s.ModelID, "kind", s.Kind, "size", s.Size, "storageGb", s.StorageGB,
			"trust", s.Trust, "catalogTier", s.Tier, "patchedFields", strs(s.PatchedFields), "unsupportedCapabilities", unsupported,
			"selected", slices.Contains(cfg.Selected[s.Kind], s.ModelID), "active", active[s.ModelID], "complete", complete,
			"capabilities", caps, "capabilityMismatches", mismatches,
			"inputModalities", object("values", strs(report.InputModalities), "source", report.InputSource),
			"capabilityEvidence", strs(report.Evidence), "runtimeBuild", build,
			"memory", object("minimumGb", float64(fit.MinVramGB), "source", fit.Source, "launchShape", fit.LaunchShape,
				"observedPeakBytes", peak, "verdict", fit.Fit, "systemMemoryGb", float64(systemGB))))
	}
	redact := func(s string) string { return strings.ReplaceAll(s, dir, "<root>") }
	issues := []any{}
	for _, d := range cat.Diagnostics {
		issue := object("tier", d.Tier, "file", filepath.Base(d.Path))
		if d.ModelID != "" {
			issue.Set("modelId", d.ModelID)
		}
		issue.Set("message", redact(d.Message))
		issues = append(issues, issue)
	}
	status := catalog.Status(dir, cat, os.Getenv, time.Now())
	feed := jsjson.FromGo(status).(*jsjson.Object)
	if status.URL != nil {
		feed.Set("url", catalog.FeedURLForDiagnostics(*status.URL))
	}
	for _, k := range []string{"problem", "lastError"} {
		if s, ok := feed.Get(k); ok && s != nil {
			feed.Set(k, redact(s.(string)))
		}
	}
	return object("models", list, "catalogIssues", issues, "catalogFeed", feed)
}

func printDiagnosticModels(c *cli.Ctx, data *jsjson.Object) {
	v, _ := data.Get("catalogFeed")
	feed := v.(*jsjson.Object)
	field := func(o *jsjson.Object, k string) any { x, _ := o.Get(k); return x }
	if field(feed, "enabled") == true {
		added, _ := field(feed, "added").([]any)
		fetched := "never downloaded"
		if f, ok := field(feed, "fetchedAt").(string); ok && f != "" {
			fetched = "fetched " + f
		}
		stale := ""
		if field(feed, "stale") == true {
			stale = ", stale"
		}
		lastErr := ""
		if e, ok := field(feed, "lastError").(string); ok && e != "" {
			lastErr = "; last error: " + e
		}
		c.Info("Catalog feed: %s (%d community model(s), %s%s)%s", termsafe.PrintableASCII(fmt.Sprint(field(feed, "url"))), len(added), fetched, stale, lastErr)
	} else {
		c.Info("Catalog feed: off")
	}
	issues, _ := field(data, "catalogIssues").([]any)
	for _, i := range issues {
		o := i.(*jsjson.Object)
		c.Info("Catalog file skipped (%v): %v: %v", field(o, "tier"), field(o, "file"), field(o, "message"))
	}
	list, _ := field(data, "models").([]any)
	for _, m := range list {
		o := m.(*jsjson.Object)
		id := field(o, "id")
		mem := field(o, "memory").(*jsjson.Object)
		c.Info("Memory for %v: %s GB (%v), %v", id, jsjson.Number(field(mem, "minimumGb").(float64)), field(mem, "source"), field(mem, "verdict"))
		if trust := field(o, "trust"); trust != "qualified" {
			patched, _ := field(o, "patchedFields").([]any)
			changed := ""
			if len(patched) > 0 {
				names := make([]string, len(patched))
				for i, p := range patched {
					names[i] = p.(string)
				}
				changed = "; changed " + strings.Join(names, ", ")
			}
			c.Info("Catalog trust for %v: %v (%v tier%s)", id, trust, field(o, "catalogTier"), changed)
		}
		unsupported, _ := field(o, "unsupportedCapabilities").([]any)
		for _, u := range unsupported {
			uo := u.(*jsjson.Object)
			c.Info("Verified unsupported for %v: %s (%v)", id, meta.CapabilityLabel(field(uo, "id").(string)), field(uo, "reason"))
		}
		mismatches, _ := field(o, "capabilityMismatches").([]any)
		if len(mismatches) == 0 {
			continue
		}
		parts := make([]string, len(mismatches))
		for i, mm := range mismatches {
			parts[i] = formatMismatch(mm.(*jsjson.Object))
		}
		c.Info("Capability mismatch for %v: %s", id, strings.Join(parts, "; "))
	}
}

func formatMismatch(m *jsjson.Object) string {
	field := func(k string) (any, bool) { return m.Get(k) }
	id, _ := field("id")
	effective, _ := field("effective")
	if reason, ok := field("unsupportedReason"); ok {
		return fmt.Sprintf("%v (verified unsupported: %v; using %v)", id, reason, effective)
	}
	declared, _ := field("declared")
	sources := []string{fmt.Sprintf("declared %v", declared)}
	for _, k := range [][2]string{{"detected", "detected"}, {"runtime", "runtime"}, {"verified", "round-trip"}} {
		if v, ok := field(k[0]); ok {
			sources = append(sources, fmt.Sprintf("%s %v", k[1], v))
		}
	}
	return fmt.Sprintf("%v (%s; using %v)", id, strings.Join(sources, ", "), effective)
}

// diagnosticsOutput resolves --output, or a fresh name in the working
// directory.
func diagnosticsOutput(requested string) (string, error) {
	if strings.ContainsAny(requested, "\x00\r\n") {
		return "", errors.New("Diagnostics output path contains invalid characters.")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	var path string
	if requested != "" {
		path = requested
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		path = filepath.Clean(path)
	} else {
		b := make([]byte, 16)
		rand.Read(b)
		b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
		x := hex.EncodeToString(b)
		path = filepath.Join(cwd, fmt.Sprintf("localbase-diagnostics-%d-%s-%s-%s-%s-%s.zip", time.Now().UnixMilli(),
			x[0:8], x[8:12], x[12:16], x[16:20], x[20:]))
	}
	if strings.ToLower(filepath.Ext(path)) != ".zip" {
		return "", errors.New("Diagnostics output must end in .zip.")
	}
	if !filepath.IsAbs(path) || path == "/" {
		return "", errors.New("Diagnostics output path is invalid.")
	}
	return path, nil
}

type zipEntry struct {
	name string
	data []byte
}

// writeDiagnosticsArchive deflates the entries and publishes the archive
// without following symlinks or replacing an existing file: a private
// temporary file in the target directory, hard-linked to the final name.
func writeDiagnosticsArchive(path string, entries []zipEntry) (int, error) {
	total := 0
	for _, e := range entries {
		total += len(e.data)
	}
	if total > maxDiagnosticsSource {
		return 0, errors.New("Diagnostics archive exceeds the uncompressed size limit.")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) { return flate.NewWriter(w, 6) })
	for _, e := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return 0, err
		}
		if _, err := w.Write(e.data); err != nil {
			return 0, err
		}
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	if buf.Len() > maxDiagnosticsZip {
		return 0, errors.New("Diagnostics archive exceeds the size limit.")
	}
	parent, err := secureParent(filepath.Dir(path))
	if err != nil {
		return 0, err
	}
	if err := safefile.PublishNew(parent, filepath.Base(path), ".localbase-diagnostics-", buf.Bytes()); err != nil {
		var pe *safefile.PublishError
		errors.As(err, &pe)
		switch pe.Step {
		case "create":
			return 0, errors.New("Secure diagnostics temporary file creation failed.")
		case "write":
			return 0, errors.New("Secure diagnostics archive write failed.")
		case "link":
			return 0, errors.New("Secure diagnostics archive publication; output already exists failed.")
		}
		// "sync": the archive is published, and the bundle is for sharing now, so it goes ahead as it
		// always did; only whether its name would survive a crash is unknown.
	}
	return buf.Len(), nil
}

// secureParent is safefile.RealDir in the diagnostics command's words.
func secureParent(dir string) (string, error) {
	got, err := safefile.RealDir(dir)
	if errors.Is(err, safefile.ErrNotAbsolute) {
		return "", errors.New("Secure output parent is invalid.")
	}
	if err != nil {
		return "", errors.New("Secure diagnostics directory open failed.")
	}
	return got, nil
}
