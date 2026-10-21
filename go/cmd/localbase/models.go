package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/install"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/meta"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// readEnv is the environment of listing commands: it never creates the root
// or the database. Without a database the defaults stand in, as TS does.
func readEnv(c *cli.Ctx, persisted bool) (*env, error) {
	dir := root.Resolve(c.Global.Root)
	e := &env{Root: &root.Root{Dir: dir}, Host: memory.NewHostProvider()}
	if r, err := root.Open(dir); err == nil {
		e.Root = r
	} else if canon, cerr := root.Canonical(dir); cerr == nil {
		e.Root = &root.Root{Dir: canon}
	}
	var err error
	if e.Catalog, err = catalog.LoadRoot(e.Root.Dir, nil); err != nil {
		return nil, err
	}
	if _, err := os.Stat(e.Root.DB()); err == nil {
		if st, err := store.OpenReadOnly(e.Root.DB()); err == nil {
			e.Store = st
			if persisted {
				if cfg, err := st.LoadConfig(context.Background()); err == nil {
					e.Config = cfg
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

func cliFit(e *env, s *catalog.Spec, memoryGB int) *runtime.Fit {
	var peaks []store.ObservedPeak
	if sha := s.Primary().SHA256; sha != "" && e.Store != nil {
		peaks, _ = e.Store.ObservedPeaks(context.Background(), s.ModelID, sha)
	}
	return runtime.FitFor(s, memoryGB, nil, peaks, e.Catalog.PlannableFeedPeaks(s))
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
		modelsCatalog(), modelsRecommend(), modelsList(), modelsInstall(), modelsShow(),
		modelsImportCommand(), modelsBackendCommand(), modelsInspectCommand(), modelsExportCommand(), modelsAddDefinitionCommand(), modelsDeleteCommand(), feedCommand(),
	}}
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
				rows = append(rows, meta.Project(context.Background(), in, s))
			}
			rows = meta.QueryWithText(rows, q, func(m meta.Model) []string {
				s := e.Catalog.ByID(m.ID)
				return []string{s.Family, s.Provider, s.Quant, s.Size, s.Catch, s.Notes}
			})
			out := []map[string]any{}
			for _, m := range rows {
				s := e.Catalog.ByID(m.ID)
				f := cliFit(e, s, memGB)
				coding := ""
				if s.CodingScore != nil {
					coding = fmt.Sprintf(" | coding=%g/10", *s.CodingScore)
				}
				c.Info("%-5s | %-38s | size=%-10s | min_vram=%3d GB %-8s | fit=%-8s | storage=%.2f GB | status=%s%s",
					s.Kind, s.ModelID, s.Size, f.MinVramGB, f.Source, f.Fit, s.StorageGB, s.CommercialStatus, coding)
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
				if s.VideoRuntime != nil && s.VideoRuntime.Mode == "s2v" {
					c.Info("      Requires Linux x64 with a single NVIDIA GPU.")
				}
				c.Info("      catch: %s", s.Catch)
				extra := memoryJSON(f)
				extra["trust"], extra["catalogTier"] = s.Trust, tier(s)
				out = append(out, specJSON(s, extra))
			}
			if len(out) == 0 && len(listed) > 0 {
				c.Info("No models match these filters.")
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
					sa, sb := 0.0, 0.0
					if a.CodingScore != nil {
						sa = *a.CodingScore
					}
					if b.CodingScore != nil {
						sb = *b.CodingScore
					}
					if sa != sb {
						return sa > sb
					}
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
				f := cliFit(e, s, vram)
				c.Info("- %s (%.2f GB, min %d GB %s, features=%s)", s.ModelID, s.StorageGB, f.MinVramGB, f.Source,
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
			ext := filepath.Ext(en.Name())
			if slices.Contains([]string{".gguf", ".bin", ".onnx", ".safetensors", ".pth"}, ext) && !known[en.Name()] {
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

// installModel checks eligibility and installs one catalog model.
func installModel(ctx context.Context, e *env, id string, report func(install.Event)) (string, error) {
	s := e.Catalog.ByID(id)
	if s == nil {
		return "", fmt.Errorf("Unknown model id: %s", id)
	}
	if reason := conf.Ineligible(s, e.Config.AllowExperimental); reason != "" {
		return "", fmt.Errorf("%s: %s", id, reason)
	}
	token := e.Config.HFToken
	return install.Model(ctx, s, e.Root.ModelsDir(s.Kind), install.Options{HFToken: token, Report: report})
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
		Examples: []string{"local-base models install qwen2.5-coder-7b-instruct-q4_k_m",
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
						fmt.Fprintf(os.Stderr, "Failed to install %q: %v\n", id, err)
						return nil, err
					}
					c.Info("Installed: %s", path)
					installed = append(installed, map[string]string{"modelId": id, "path": path})
				}
				c.Info("\nAll selected models installed successfully.")
				return map[string]any{"installed": installed}, nil
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
			c.Info("Installed: %s", path)
			return map[string]any{"installed": []map[string]string{{"modelId": id, "path": path}}}, nil
		}}
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
		Examples: []string{"local-base models show qwen2.5-coder-7b-instruct-q4_k_m", "local-base --json models show whisper-large-v3-turbo"},
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
			var voices []store.Voice
			if s.Kind == "tts" && e.Store != nil {
				voices, _ = e.Store.Voices(context.Background())
			}
			m := meta.Project(context.Background(), meta.Input{Store: e.Store, RootDir: e.Root.Dir, Config: e.Config, Catalog: e.Catalog,
				MemoryGB: e.memoryGB(), Voices: voices, CLI: true}, s)
			isDefault := false
			for _, k := range kinds {
				isDefault = isDefault || e.Config.Active[k] == s.ModelID
			}
			path := s.LayerPath // the contributing file or feed position (TS provenance.path)
			for _, l := range meta.DetailLines(m, m.Device.Installed, m.Device.Selected, isDefault, path) {
				c.Info("%s", l)
			}
			return map[string]any{"model": m, "status": map[string]bool{"installed": m.Device.Installed,
				"enabled": m.Device.Selected, "default": isDefault}}, nil
		}}
}
