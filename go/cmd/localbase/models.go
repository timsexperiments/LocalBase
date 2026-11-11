package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/backup"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/install"
	"github.com/timsexperiments/LocalBase/go/internal/instance"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/meta"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// readEnv is the environment of listing commands: it never creates the root
// or the database. Without a database the defaults stand in, as TS does.
func readEnv(c *cli.Ctx, persisted bool) (*env, error) {
	e, err := readEnvKeepingProblems(c, persisted)
	if err != nil {
		return nil, err
	}
	if e.DatabaseProblem != nil {
		return nil, e.DatabaseProblem
	}
	return e, nil
}

// readEnvKeepingProblems is readEnv for doctor, which still has things to say about a database it
// can't read: the problem is in DatabaseProblem, with no Store and default settings.
func readEnvKeepingProblems(c *cli.Ctx, persisted bool) (*env, error) {
	if err := checkOtelEnvironment(); err != nil {
		return nil, err
	}
	dir := root.Resolve(c.Global.Root)
	e := &env{Root: &root.Root{Dir: dir}, Host: newHostProvider()} // the seam exists only so tests can swap the host
	if r, err := root.Open(dir); err == nil {
		e.Root = r
	} else if canon, cerr := root.Canonical(dir); cerr == nil {
		e.Root = &root.Root{Dir: canon}
	}
	e.audit = NewAuditLog(e.Root, nil) // a record it can't write is reported on stderr
	// Read-only, this cannot undo a write a process left unfinished, and what it reads below
	// would quietly be defaults, reported as the real settings.
	if msg := backup.UnfinishedWrite(context.Background(), e.Root.Dir); msg != "" {
		return nil, errors.New(msg)
	}
	var err error
	if e.Catalog, err = catalog.LoadRoot(e.Root.Dir, nil); err != nil {
		return nil, err
	}
	if _, err := os.Stat(e.Root.DB()); err == nil {
		// There is a database, so it has settings, and defaults reported as if they were those would
		// be wrong, and exit 0.
		st, err := store.OpenReadOnly(e.Root.DB())
		if err != nil {
			e.DatabaseProblem = &unreadableSettings{Root: e.Root.Dir, Err: &failedRead{Step: "opening the database", Err: err}}
		} else {
			e.Store = st
			if persisted {
				afterReadOpen()
				if e.Config, err = persistedConfig(st); err != nil {
					_ = st.Close() // Read-only.
					e.Store, e.Config, e.DatabaseProblem = nil, nil, &unreadableSettings{Root: e.Root.Dir, Err: err}
				}
			}
		}
	}
	if e.Config == nil {
		e.Config = conf.Default(e.Root.Dir, e.memoryGB(), e.Catalog)
	}
	return e, nil
}

var kinds = []string{"llm", "stt", "tts", "image", "video"}

func kindOption() cli.Option {
	return cli.Option{Name: "kind", Value: "<llm|stt|tts|image|video>", Help: "Model kind"}
}

func kindArg(c *cli.Ctx) (string, error) {
	k := c.String("kind")
	if k != "" && !slices.Contains(kinds, k) {
		return "", cli.Inputf("kind: Invalid option: expected one of \"llm\"|\"stt\"|\"tts\"|\"image\"|\"video\"")
	}
	return k, nil
}

// memoryJSON is the additive memory fields of CLI model listings.
func memoryJSON(f *runtime.Fit) map[string]any {
	var peak any
	if f.ObservedPeakBytes != nil {
		peak = *f.ObservedPeakBytes
	}
	return map[string]any{"minVramGb": f.MinVramGB, "minimumVramEstimateGb": f.MinVramGB, "memorySource": f.Source,
		"memoryLaunchShape": f.LaunchShape, "observedPeakBytes": peak, "fitVerdict": f.Fit,
		"systemMemoryGb": f.SystemMemoryGB}
}

// specJSON is a catalog spec plus the legacy features list.
func specJSON(s *catalog.Spec, extra map[string]any) map[string]any {
	b, _ := json.Marshal(s)
	var m map[string]any
	json.Unmarshal(b, &m)
	m["features"] = meta.Features(s)
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func cliFit(e *env, s *catalog.Spec, memoryGB int) (*runtime.Fit, error) {
	var peaks []store.ObservedPeak
	if sha := s.Primary().SHA256; sha != "" && e.Store != nil {
		var err error
		if peaks, err = e.Store.ObservedPeaks(context.Background(), s.ModelID, sha); err != nil {
			return nil, &failedRead{Step: "reading the memory peaks seen for " + s.ModelID, Err: err}
		}
	}
	return runtime.FitFor(s, memoryGB, nil, peaks, e.Catalog.PlannableFeedPeaks(s)), nil
}

// cliVoices is the custom voices a speech model's page lists.
func cliVoices(e *env, s *catalog.Spec) ([]store.Voice, error) {
	if s.Kind != "tts" || e.Store == nil {
		return nil, nil
	}
	voices, err := e.Store.Voices(context.Background())
	if err != nil {
		return nil, &failedRead{Step: "reading the custom voices", Err: err}
	}
	return voices, nil
}

func tier(s *catalog.Spec) string {
	if s.Tier != "" {
		return s.Tier
	}
	if s.Trust == "qualified" {
		return "built-in"
	}
	return "user"
}

func modelsCommand() *cli.Command {
	return &cli.Command{Name: "models", Summary: "Browse and install models", Sub: []*cli.Command{
		modelsCatalog(), modelsRecommend(), modelsList(), modelsInstall(), modelsShow(), modelsAdd(), modelsSearch(), modelsRemoveEntry(),
		modelsImportCommand(), modelsBackendCommand(), modelsInspectCommand(), modelsExportCommand(), modelsAddDefinitionCommand(), modelsDeleteCommand(), feedCommand(),
		modelsPlacementCommand(), modelsForgetPeaks(), modelsCancel(),
	}}
}

// modelsForgetPeaks is the way out of a peak that no longer applies: admission uses the highest one
// seen, and nothing else ever lowers it.
func modelsForgetPeaks() *cli.Command {
	return &cli.Command{Name: "forget-peaks", Summary: "Forget the memory peaks seen for a model on this machine", Usage: "<modelId>", MinArgs: 1, MaxArgs: 1,
		Examples: []string{"local-base models forget-peaks whisper-large-v3-turbo"},
		Run: func(c *cli.Ctx) (any, error) {
			e, err := openEnv(c)
			if err != nil {
				return nil, err
			}
			defer e.Close()
			id := c.Args[0]
			n, err := e.Store.ForgetObservedPeaks(context.Background(), id)
			if err != nil {
				return nil, err
			}
			if n == 0 {
				c.Info("No memory peaks are recorded for %s.", id)
			} else {
				// It changes what admission allows this model, so it's on record, like placement.
				e.audit.Change("models", "forget-peaks", "model", id, "count", n)
				c.Info("Forgot %d memory peaks for %s. Its next load is sized from its estimate, and a model that is loaded now keeps recording.", n, id)
			}
			return map[string]any{"modelId": id, "forgotten": n}, nil
		}}
}

// videoCatalogLine is the note for a video model no supported computer can run.
func videoCatalogLine(s *catalog.Spec) string {
	if s.VideoRuntime != nil && !s.VideoRunsAnywhere() {
		return catalog.VideoModelRefusal
	}
	return ""
}

// modelsCancel stops an install the running gateway is doing, as POST /_localbase/model-management
// does, and answers with the same operation.
func modelsCancel() *cli.Command {
	return &cli.Command{Name: "cancel", Summary: "Stop an install the running gateway is doing", Usage: "<modelId>", MinArgs: 1, MaxArgs: 1,
		Examples: []string{"local-base models cancel qwen3.5-4b-q4_k_m"},
		Run: func(c *cli.Ctx) (any, error) {
			e, err := readEnv(c, false)
			if err != nil {
				return nil, err
			}
			defer e.Close()
			gw, state, detail := instance.Dial(e.Root) // the cancel goes only to a proven gateway
			if state == "unknown" || state == "invalid" {
				if detail == "" {
					detail = "its lock could not be read"
				}
				return nil, errors.New("Cannot tell whether a gateway is running on this root: " + detail + ". Nothing changed.")
			}
			if gw == nil {
				return nil, errors.New("No gateway is running on this root, so it isn't installing anything. An install started with local-base models install stops with Ctrl-C in its terminal.")
			}
			op, err := cancelInstall(gw, c.Args[0])
			if err != nil {
				return nil, err
			}
			if op["state"] == "running" {
				c.Info("%s is still stopping. It will show as cancelled in a moment.", c.Args[0])
			} else if detail, _ := op["detail"].(string); detail != "" {
				c.Info("%s", detail)
			}
			return op, nil
		}}
}

func cancelInstall(gw *instance.Client, modelID string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a, err := gw.Post(ctx, "/_localbase/model-management", map[string]string{"modelId": modelID, "action": "cancel"})
	if err != nil {
		return nil, errors.New("A gateway is running on this root, but it did not answer. Nothing changed. Try again.")
	}
	var out map[string]any
	decodeErr := a.Decode(&out)
	switch {
	case (a.Status == http.StatusOK || a.Status == http.StatusAccepted) && decodeErr == nil:
		return out, nil
	case a.Status == http.StatusConflict || a.Status == http.StatusBadRequest:
		if e, ok := out["error"].(map[string]any); ok {
			if message, ok := e["message"].(string); ok {
				return nil, errors.New(message)
			}
		}
	}
	return nil, fmt.Errorf("The gateway running on this root did not accept this (HTTP %d). It may be an older version: run local-base restart.", a.Status)
}

func modelsCatalog() *cli.Command {
	return &cli.Command{Name: "catalog", Summary: "List all supported models",
		Examples: []string{"local-base models catalog --capability tool-calling --fits",
			"local-base models catalog --kind image --sort memory",
			"local-base models catalog --installed --search qwen --sort context"},
		Options: []cli.Option{kindOption(),
			{Name: "capability", Value: "<id>", Repeatable: true, Help: "Only models with this effective capability (repeatable or comma-separated)"},
			{Name: "fits", Bool: true, Help: "Only models that fit this machine, including tight fits"},
			{Name: "fit", Value: "<fits|tight|wont-fit>", Repeatable: true, Help: "Only models with this fit verdict (repeatable)"},
			{Name: "installed", Bool: true, Help: "Only installed models (--no-installed: only models that are not)"},
			{Name: "license", Value: "<open|conditional|prohibited>", Repeatable: true, Help: "Only models with this commercial-use status (repeatable)"},
			{Name: "search", Value: "<text>", Help: "Match id, name, and description"},
			{Name: "sort", Value: "<name|size|memory|fit|context>", Help: "Sort by name, size, memory, fit, or context length"},
			{Name: "reverse", Bool: true, Help: "Reverse the order"},
		},
		Run: func(c *cli.Ctx) (any, error) {
			kind, err := kindArg(c)
			if err != nil {
				return nil, err
			}
			if s := c.String("sort"); s != "" && !slices.Contains([]string{"name", "size", "memory", "fit", "context"}, s) {
				return nil, cli.Inputf("sort: Invalid option: expected one of \"name\"|\"size\"|\"memory\"|\"fit\"|\"context\"")
			}
			e, err := readEnv(c, false)
			if err != nil {
				return nil, err
			}
			defer e.Close()
			memGB := e.memoryGB()
			q := url.Values{}
			if c.Bool("fits") {
				q["fit"] = []string{"fits,tight"}
			} else {
				q["fit"] = c.Strings("fit")
			}
			q["capability"], q["license"] = c.Strings("capability"), c.Strings("license")
			q.Set("q", c.String("search"))
			if v, ok := c.OptBool("installed"); ok {
				q.Set("installed", fmt.Sprint(v))
			}
			q.Set("sort", c.String("sort"))
			if c.Bool("reverse") {
				q.Set("reverse", "true")
			}
			if kind != "" {
				q.Set("kind", kind)
			}
			in := meta.Input{Store: e.Store, RootDir: e.Root.Dir, Config: e.Config, MemoryGB: memGB, CLI: true, Catalog: e.Catalog}
			var rows []meta.Model
			var listed []*catalog.Spec
			for _, s := range e.Catalog.All() {
				if (kind != "" && s.Kind != kind) || (s.QualificationState == "experimental" && !e.Config.AllowExperimental) {
					continue
				}
				listed = append(listed, s)
				m, problems := meta.Project(context.Background(), in, s)
				if len(problems) > 0 {
					return nil, problems[0]
				}
				rows = append(rows, m)
			}
			rows = meta.QueryWithText(rows, q, func(m meta.Model) []string {
				s := e.Catalog.ByID(m.ID)
				return []string{s.Family, s.Provider, s.Quant, s.Size, s.Catch, s.Notes}
			})
			out := []map[string]any{}
			for _, m := range rows {
				s := e.Catalog.ByID(m.ID)
				f, err := cliFit(e, s, memGB)
				if err != nil {
					return nil, err
				}
				c.Info("%-5s | %-38s | size=%-10s | plan_min=%3d GB %-8s | fit=%-8s | storage=%.2f GB | status=%s",
					s.Kind, s.ModelID, s.Size, f.MinVramGB, f.Source, f.Fit, s.StorageGB, s.CommercialStatus)
				line := fmt.Sprintf("      in=%s out=%s features=%s", strings.Join(s.InputModalities, ","),
					strings.Join(s.OutputModalities, ","), strings.Join(meta.Features(s), ","))
				if s.QualificationState == "experimental" {
					line += " qualification=experimental (unqualified)"
				}
				if s.Trust != "qualified" {
					line += " trust=" + s.Trust + " source=" + tier(s)
				}
				c.Info("%s", line)
				for _, u := range s.UnsupportedCapabilities {
					c.Info("      unsupported: %s (verified: %s)", u.ID, u.Reason)
				}
				if f.Source == "observed" {
					c.Info("      memory: %d GB (observed peak %.1f GB at %s; %s %.1f GB)", f.MinVramGB,
						float64(f.DemandBytes)/float64(memory.GiB), f.LaunchShape, f.ReferenceSource, float64(f.ReferenceBytes)/float64(memory.GiB))
				}
				if line := videoCatalogLine(s); line != "" {
					c.Info("      %s", line)
				}
				c.Info("      catch: %s", s.Catch)
				extra := memoryJSON(f)
				extra["trust"], extra["catalogTier"] = s.Trust, tier(s)
				out = append(out, specJSON(s, extra))
			}
			if len(out) == 0 && len(listed) > 0 {
				c.Info("No models match these filters.")
			}
			if len(out) > 0 {
				c.Info("plan_min: %s Each figure is at the model's reference launch (LLMs: up to an 8,192-token context, 1 request at a time); \"measured\" figures were recorded on %s. Run \"local-base models show <id>\" for the exact settings.",
					meta.PlanningMinimumNote, meta.MeasurementHardware)
			}
			return map[string]any{"models": out}, nil
		}}
}

func modelsRecommend() *cli.Command {
	return &cli.Command{Name: "recommend", Summary: "Recommend models for available VRAM",
		Options: []cli.Option{kindOption(), {Name: "vram", Value: "<GB>", Help: "Target VRAM in GB"}},
		Run: func(c *cli.Ctx) (any, error) {
			kind, err := kindArg(c)
			if err != nil {
				return nil, err
			}
			if kind == "" {
				kind = "llm"
			}
			e, err := readEnv(c, false)
			if err != nil {
				return nil, err
			}
			defer e.Close()
			vram := e.memoryGB()
			if v, ok, err := c.Int("vram"); err != nil {
				return nil, err
			} else if ok {
				if v <= 0 {
					return nil, cli.Inputf("vram: must be a positive integer")
				}
				vram = v
			}
			var picks []*catalog.Spec
			for _, s := range e.Catalog.All() {
				if s.Kind != kind || kind == "video" || runtime.FitFor(s, 0, nil, nil, e.Catalog.PlannableFeedPeaks(s)).MinVramGB > vram {
					continue
				}
				if kind == "llm" && s.LLMRuntime != nil {
					continue
				}
				picks = append(picks, s)
			}
			minGB := func(s *catalog.Spec) int {
				return runtime.FitFor(s, 0, nil, nil, e.Catalog.PlannableFeedPeaks(s)).MinVramGB
			}
			sort.SliceStable(picks, func(i, j int) bool {
				a, b := picks[i], picks[j]
				if kind == "llm" {
					return minGB(a) > minGB(b)
				}
				return a.StorageGB < b.StorageGB
			})
			if len(picks) > 6 {
				picks = picks[:6]
			}
			c.Info("Recommended %s models for <= %dGB VRAM:", strings.ToUpper(kind), vram)
			out := []map[string]any{}
			for _, s := range picks {
				f, err := cliFit(e, s, vram)
				if err != nil {
					return nil, err
				}
				c.Info("- %s (%.2f GB, planning min %d GB %s, features=%s)", s.ModelID, s.StorageGB, f.MinVramGB, f.Source,
					strings.Join(meta.Features(s), "/"))
				out = append(out, specJSON(s, memoryJSON(f)))
			}
			return map[string]any{"kind": kind, "vramGb": vram, "models": out}, nil
		}}
}

// installedModels lists complete catalog installs plus loose model files
// (`kind:name` without --kind), sorted.
func installedModels(e *env, kind string) []string {
	found := []string{}
	for _, k := range kinds {
		if kind != "" && k != kind {
			continue
		}
		dir := e.Root.ModelsDir(k)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		known := map[string]bool{}
		var names []string
		for _, s := range e.Catalog.All() {
			if s.Kind != k {
				continue
			}
			for _, a := range s.Artifacts {
				known[a.Filename] = true
			}
			if complete, _ := s.Installation(dir); complete {
				names = append(names, s.ModelID)
			}
		}
		for _, en := range entries {
			if looseModelFile(dir, en.Name()) && !known[en.Name()] {
				names = append(names, en.Name())
			}
		}
		for _, n := range names {
			if kind == "" {
				n = k + ":" + n
			}
			found = append(found, n)
		}
	}
	sort.Strings(found)
	return found
}

func modelsList() *cli.Command {
	return &cli.Command{Name: "list", Summary: "List installed models", Options: []cli.Option{kindOption()},
		Run: func(c *cli.Ctx) (any, error) {
			kind, err := kindArg(c)
			if err != nil {
				return nil, err
			}
			e, err := openEnv(c)
			if err != nil {
				return nil, err
			}
			defer e.Close()
			found := installedModels(e, kind)
			if len(found) == 0 {
				if kind != "" {
					c.Info("No installed %s models found.", strings.ToUpper(kind))
				} else {
					c.Info("No installed models found.")
				}
			}
			for _, f := range found {
				c.Info("%s", f)
			}
			return map[string]any{"models": found}, nil
		}}
}

// currentHFToken is the saved token now, not when this process started: serve installs on demand
// long after it read its configuration. If the read fails, what it started with is the best left.
func currentHFToken(ctx context.Context, e *env) string {
	if saved, err := e.Store.LoadConfig(ctx); err == nil {
		return saved.HFToken
	}
	return e.Config.HFToken
}

// installModel checks eligibility and installs one catalog model.
func installModel(ctx context.Context, e *env, id string, report func(install.Event)) (string, error) {
	s := e.Catalog.ByID(id)
	if s == nil {
		return "", fmt.Errorf("Unknown model id: %s", id)
	}
	if reason := conf.Ineligible(s, e.Config.AllowExperimental); reason != "" {
		return "", fmt.Errorf("%s: %s", id, reason)
	}
	path, err := install.Model(ctx, s, e.Root.ModelsDir(s.Kind), install.Options{HFToken: currentHFToken(ctx, e), AllowPrivate: e.Store.PrivateImportsAllowed(ctx), Report: report})
	if err != nil {
		return "", &cli.ReasonedError{Err: err, Reason: install.Reason(err)}
	}
	return path, nil
}

// progressReporter prints download progress on a terminal, as TS does.
func progressReporter(c *cli.Ctx) func(install.Event) {
	tty := isTerminal(os.Stderr)
	last := -1
	return func(ev install.Event) {
		switch ev.Kind {
		case "download-progress":
			if tty && ev.Percent != last {
				last = ev.Percent
				fmt.Fprintf(os.Stderr, "\r   Download progress: %d%%", ev.Percent)
				if ev.Downloaded == ev.Total {
					fmt.Fprintln(os.Stderr)
				}
			}
		case "verification-started":
			c.Info("Verifying checksum for %s...", ev.Filename)
		}
	}
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func modelsInstall() *cli.Command {
	return &cli.Command{Name: "install", Summary: "Download and install a model", Usage: "[modelId]", MaxArgs: 1,
		Examples: []string{"local-base models install qwen3.5-4b-q4_k_m",
			"local-base --non-interactive models install --all"},
		Options: []cli.Option{{Name: "all", Bool: true, Help: "Install all selected models"}},
		Run: func(c *cli.Ctx) (any, error) {
			e, err := openEnv(c)
			if err != nil {
				return nil, err
			}
			defer e.Close()
			ctx := context.Background()
			installed := []map[string]string{}
			if c.Bool("all") {
				var ids []string
				for _, k := range kinds {
					ids = append(ids, e.Config.Selected[k]...)
				}
				if len(ids) == 0 {
					c.Info("No models selected in the configuration to install.")
					return map[string]any{"installed": installed}, nil
				}
				c.Info("Installing all %d selected models...", len(ids))
				for _, id := range ids {
					if e.Catalog.ByID(id) == nil {
						c.Info("Skipping %q: Model does not exist in the catalog.", id)
						continue
					}
					path, err := installModel(ctx, e, id, progressReporter(c))
					if err != nil {
						c.Error("Failed to install %q: %v", id, err)
						return nil, err
					}
					e.audit.Change("models", "install", "model", id)
					c.Info("Installed: %s", path)
					installed = append(installed, map[string]string{"modelId": id, "path": path})
				}
				if err := activateInstalled(c, e, ids...); err != nil {
					return nil, err
				}
				c.Info("\nAll selected models installed successfully.")
				return e.audit.WithResult(map[string]any{"installed": installed}), nil
			}
			if len(c.Args) == 0 {
				return nil, cli.Inputf("Provide a model ID or --all.")
			}
			id := c.Args[0]
			if e.Catalog.ByID(id) == nil {
				return nil, cli.Inputf("Unknown model id: %s", id)
			}
			path, err := installModel(ctx, e, id, progressReporter(c))
			if err != nil {
				return nil, err
			}
			e.audit.Change("models", "install", "model", id)
			c.Info("Installed: %s", path)
			if err := activateInstalled(c, e, id); err != nil {
				return nil, err
			}
			return e.audit.WithResult(map[string]any{"installed": []map[string]string{{"modelId": id, "path": path}}}), nil
		}}
}

// activateInstalled makes the first of these chat models that can run the
// default when the current default cannot (conf.ActivateFirstInstall).
func activateInstalled(c *cli.Ctx, e *env, ids ...string) error {
	present := installedModels(e, "llm")
	isInstalled := func(s *catalog.Spec) bool { return slices.Contains(present, s.ModelID) }
	for _, id := range ids {
		s := e.Catalog.ByID(id)
		if s == nil || !conf.ActivateFirstInstall(e.Config, s, e.Catalog, isInstalled) {
			continue
		}
		if err := e.save(e.Config); err != nil {
			return err
		}
		e.audit.Change("default-models", "set", "model", id)
		c.Info("%s is now the default LLM.", id)
		return nil
	}
	return nil
}

// installer is serve's --install-missing hook: nil without consent, so the
// LLM manager answers 409 model_install_consent_required.
func installer(e *env, allowed bool, log *slog.Logger) func(context.Context, string) error {
	if !allowed {
		return nil
	}
	return func(ctx context.Context, id string) error {
		log.Info("Installing selected model "+id+".", "event", "models.install-started", "model_id", id)
		_, err := installModel(ctx, e, id, func(ev install.Event) {
			if ev.Kind == "completed" {
				log.Info("Selected model "+id+" installation completed.", "event", "models.install-completed", "model_id", id)
			}
		})
		if err != nil {
			log.Error("Selected model "+id+" installation failed.", "event", "models.install-failed", "model_id", id, "error", err.Error())
		}
		return err
	}
}

func modelsShow() *cli.Command {
	return &cli.Command{Name: "show", Summary: "Show everything known about one model", Usage: "<modelId>", MinArgs: 1, MaxArgs: 1,
		Examples: []string{"local-base models show qwen3.5-4b-q4_k_m", "local-base --json models show whisper-large-v3-turbo"},
		Run: func(c *cli.Ctx) (any, error) {
			e, err := readEnv(c, true)
			if err != nil {
				return nil, err
			}
			defer e.Close()
			s := e.Catalog.ByID(c.Args[0])
			if s == nil {
				return nil, cli.Inputf("Unknown model id: %s", c.Args[0])
			}
			voices, err := cliVoices(e, s)
			if err != nil {
				return nil, err
			}
			m, problems := meta.Project(context.Background(), meta.Input{Store: e.Store, RootDir: e.Root.Dir, Config: e.Config, Catalog: e.Catalog,
				MemoryGB: e.memoryGB(), Voices: voices, CLI: true}, s)
			if len(problems) > 0 {
				return nil, problems[0]
			}
			isDefault := false
			for _, k := range kinds {
				isDefault = isDefault || e.Config.Active[k] == s.ModelID
			}
			path := s.LayerPath // the contributing file or feed position (TS provenance.path)
			fit, fitOK := "", false
			if s.Kind == "llm" && !c.Global.JSON {
				fit, fitOK = machineFitLine(e, s.ModelID)
			}
			for _, l := range meta.DetailLines(m, m.Device.Installed, m.Device.Selected, isDefault, path) {
				if fitOK && strings.HasPrefix(l, "  This machine:") {
					l = fit
				}
				c.Info("%s", l)
			}
			// The CPU/GPU placement (DESIGN §20), text only: the JSON stays
			// TS's shape; `models placement --json` has the details.
			if s.Kind == "llm" && !c.Global.JSON {
				for _, l := range showPlacementLines(e, s.ModelID) {
					c.Info("%s", l)
				}
			}
			return map[string]any{"model": m, "status": map[string]bool{"installed": m.Device.Installed,
				"enabled": m.Device.Selected, "default": isDefault}}, nil
		}}
}

// looseModelFile reports whether a models directory entry is a model file
// someone placed there: a regular file (or a link to one) with a model
// extension. Dot-prefixed entries are staging (benchmark parts, partial
// downloads, checksum caches) and directories are never models, even when
// their name ends in .gguf.
func looseModelFile(dir, name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	if !slices.Contains([]string{".gguf", ".bin", ".onnx", ".safetensors", ".pth"}, filepath.Ext(name)) {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && info.Mode().IsRegular()
}
