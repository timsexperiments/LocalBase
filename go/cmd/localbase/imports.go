package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/root"
)

// Model import, backend selection, backend registration and auth profiles
// (DESIGN §12). These commands are Go additions; TS has no equivalent.

func openLibrary(e *env) (*library.Library, error) {
	return library.Open(e.Root, e.Store, e.Catalog, backends.ProbeAccelerators(backends.ProbeOptions{}))
}

func withLibrary(run func(c *cli.Ctx, e *env, l *library.Library) (any, error)) func(c *cli.Ctx) (any, error) {
	return withEnv(func(c *cli.Ctx, e *env) (any, error) {
		l, err := openLibrary(e)
		if err != nil {
			return nil, err
		}
		return run(c, e, l)
	})
}

func hfToken(e *env) string {
	if e.Config != nil && e.Config.HFToken != "" {
		return e.Config.HFToken
	}
	return os.Getenv("HF_TOKEN")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}

// printSelection explains which backend runs a model, with every option.
func printSelection(c *cli.Ctx, s backends.Selection, reg *backends.Registry) {
	switch {
	case s.Unavailable:
		c.Info("Backend: %s (manual, no longer registered; choose a replacement)", s.BackendID)
	case s.BackendID == "":
		c.Info("Backend: none selected. Select a backend to run this model: local-base models backend <id> --set <backend>")
	default:
		c.Info("Backend: %s (%s, %s)", s.BackendID, s.Mode, s.Finding.Rating)
		for _, r := range s.Finding.Reasons {
			c.Info("  - %s", r.Message)
		}
		for _, w := range s.Finding.Warnings {
			c.Info("  ! %s", w.Message)
		}
		if s.DownloadBytes > 0 {
			c.Info("  ! downloading them needs %s", humanBytes(s.DownloadBytes))
		}
	}
	if s.Mode == backends.ModeManual && s.Recommendation.BackendID != "" && s.Recommendation.BackendID != s.BackendID {
		c.Info("Automatic choice on this device would be %s.", s.Recommendation.BackendID)
	}
	c.Info("Options:")
	for _, o := range s.Recommendation.Options {
		mark := " "
		if o.BackendID == s.BackendID {
			mark = "*"
		}
		name := o.BackendID
		if d := reg.Get(o.BackendID); d != nil {
			name = d.Name + " [" + d.ID + "]"
		}
		detail := ""
		if len(o.Reasons) > 0 {
			detail = ": " + o.Reasons[0].Message
		}
		c.Info(" %s %-48s %-12s %s%s", mark, name, o.Rating, o.Install, detail)
	}
}

func modelsImportCommand() *cli.Command {
	return &cli.Command{Name: "import", Summary: "Import a model from Hugging Face, a URL or a local file", Usage: "<source>", MinArgs: 1, MaxArgs: 1,
		Examples: []string{
			"local-base models import hf:Qwen/Qwen3-0.6B-GGUF --file Qwen3-0.6B-Q8_0.gguf",
			"local-base models import https://example.com/model.gguf --auth-profile vendor --task chat --yes",
			"local-base models import ./model.gguf --task embeddings",
		},
		Options: []cli.Option{
			{Name: "file", Value: "<path>", Repeatable: true, Help: "Hugging Face: the file(s) to import; companions are added"},
			{Name: "revision", Value: "<rev>", Help: "Hugging Face branch, tag or commit (resolved to a commit)"},
			{Name: "sha256", Value: "<hex>", Help: "A checksum you trust for a single file; a mismatch stops the import"},
			{Name: "auth-profile", Value: "<id>", Help: "Send this auth profile's headers to its origins"},
			{Name: "task", Value: "<task>", Help: "What the model does, when the source does not say (" + strings.Join(models.Tasks, ", ") + ")"},
			{Name: "backend", Value: "<id|auto>", Help: "Backend to run it with on this device (default: automatic)"},
			{Name: "id", Value: "<model-id>", Help: "Model id (default: derived from the file name)"},
			{Name: "name", Value: "<name>", Help: "Display name"},
			{Name: "allow-http", Bool: true, Help: "Allow a plain HTTP source (insecure transport)"},
			{Name: "yes", Alias: "y", Bool: true, Help: "Confirm a custom-source download after reading its warning"},
			{Name: "dry-run", Bool: true, Help: "Resolve and show the plan without downloading"},
		},
		Run: withLibrary(func(c *cli.Ctx, e *env, l *library.Library) (any, error) {
			ctx := context.Background()
			backend := c.String("backend")
			if backend != "" && backend != "auto" && l.Registry.Get(backend) == nil {
				return nil, cli.Inputf("backend %q is not registered (see local-base backends list)", backend)
			}
			if t := c.String("task"); t != "" && !slices.Contains(models.Tasks, t) {
				return nil, cli.Inputf("task must be one of %s", strings.Join(models.Tasks, ", "))
			}
			plan, err := acquire.Resolve(ctx, e.Root.Dir, acquire.Request{Source: c.Args[0], Files: c.Strings("file"),
				Revision: c.String("revision"), SHA256: c.String("sha256"), AuthProfile: c.String("auth-profile"),
				AllowHTTP: c.Bool("allow-http"), ID: c.String("id"), Name: c.String("name"), Task: c.String("task"), HFToken: hfToken(e)})
			if err != nil {
				return nil, err
			}
			c.Info("Source: %s", plan.Display)
			if plan.Revision != "" {
				c.Info("Revision: %s", plan.Revision)
			}
			for _, w := range plan.Warnings {
				c.Info("Warning: %s", w.Text)
			}
			if len(plan.Choices) > 0 {
				return nil, cli.Inputf("the repository has several weight files; choose one with --file: %s", strings.Join(plan.Choices, ", "))
			}
			for _, f := range plan.Files {
				size := "size unknown"
				if f.SizeBytes > 0 {
					size = humanBytes(f.SizeBytes)
				}
				trust := "checksum computed after download (identity only)"
				switch f.HashTrust {
				case models.HashAuthoritative:
					trust = "publisher checksum"
				case models.HashUser:
					trust = "your checksum"
				}
				c.Info("  %s (%s, %s, %s)", f.Filename, f.Role, size, trust)
			}
			if plan.SizeKnown {
				c.Info("Total: %s", humanBytes(plan.TotalBytes))
			}
			preview := l.Preview(plan)
			switch {
			case backend != "" && backend != "auto":
				c.Info("Backend: %s (your choice)", backend)
			case preview.BackendID != "":
				c.Info("Backend: %s (automatic, from the source's metadata; checked again after download)", preview.BackendID)
			default:
				c.Info("Backend: none detected yet. The import can finish without one; choose one later with local-base models backend.")
			}
			out := map[string]any{"plan": plan, "recommendation": preview}
			if c.Bool("dry-run") {
				return out, nil
			}
			if plan.Custom && !c.Bool("yes") {
				return nil, cli.Inputf("this is a custom source; read the warning above and re-run with --yes to download it")
			}
			if plan.Task == "" {
				return nil, cli.Inputf("the source does not say what the model does; choose --task (%s)", strings.Join(models.Tasks, ", "))
			}
			var res *acquire.Result
			err = root.WithOperation(e.Root.Dir, "install model", func() error {
				last := ""
				res, err = acquire.Import(ctx, e.Root.Dir, plan, func(p acquire.Progress) {
					if c.Global.JSON {
						return
					}
					line := p.Phase + " " + p.File
					if p.Total > 0 {
						line = fmt.Sprintf("%s %s %d%%", p.Phase, p.File, p.Done*100/p.Total)
					}
					if line != last {
						fmt.Fprintln(c.Stderr, line)
						last = line
					}
				})
				return err
			})
			if err != nil {
				return nil, err
			}
			sel, err := l.SaveImport(ctx, res, backend)
			if err != nil {
				return nil, err
			}
			c.Info("Imported %s into %s.", res.Model.ID, res.Dir)
			c.Info("Downloading a model does not prove it runs; the backend below starts it on first use.")
			printSelection(c, sel, l.Registry)
			out["model"], out["selection"] = res.Model, sel
			return out, nil
		})}
}

func modelsBackendCommand() *cli.Command {
	return &cli.Command{Name: "backend", Summary: "Show or choose the backend that runs a model on this device", Usage: "<modelId>", MinArgs: 1, MaxArgs: 1,
		Options: []cli.Option{
			{Name: "set", Value: "<backend>", Help: "Run the model with this registered backend (overrides detection on this device)"},
			{Name: "variant", Value: "<variant>", Help: "With --set: the variant to run"},
			{Name: "auto", Bool: true, Help: "Reset to automatic selection"},
		},
		Run: withLibrary(func(c *cli.Ctx, e *env, l *library.Library) (any, error) {
			ctx := context.Background()
			id := c.Args[0]
			if c.Has("set") && c.Bool("auto") {
				return nil, cli.Inputf("use either --set or --auto")
			}
			var s backends.Selection
			var err error
			switch {
			case c.Has("set"):
				s, err = l.SetBackend(ctx, id, c.String("set"), c.String("variant"))
			case c.Bool("auto"):
				s, err = l.ResetBackend(ctx, id)
			default:
				var en *library.Entry
				if en, err = l.Get(ctx, id); err == nil {
					s, err = l.Selection(ctx, en)
				}
			}
			if errors.Is(err, library.ErrNotFound) {
				return nil, cli.Inputf("model %q is not in the catalog or imported", id)
			}
			if err != nil {
				return nil, err
			}
			printSelection(c, s, l.Registry)
			return map[string]any{"modelId": id, "selection": s}, nil
		})}
}

func modelsInspectCommand() *cli.Command {
	return &cli.Command{Name: "inspect", Summary: "Read a model's files and rate it against every backend", Usage: "<modelId>", MinArgs: 1, MaxArgs: 1,
		Run: withLibrary(func(c *cli.Ctx, e *env, l *library.Library) (any, error) {
			en, err := l.Get(context.Background(), c.Args[0])
			if errors.Is(err, library.ErrNotFound) {
				return nil, cli.Inputf("model %q is not in the catalog or imported", c.Args[0])
			}
			if err != nil {
				return nil, err
			}
			ins := l.Inspect(en)
			for _, v := range en.Model.Variants {
				in := ins[v.ID]
				fact := func(name string, known bool, value any, source string) {
					if known {
						c.Info("  %-14s %v (%s)", name, value, source)
					} else {
						c.Info("  %-14s unknown", name)
					}
				}
				c.Info("Variant %s:", v.ID)
				iv := in.Variant
				fact("format", iv.Format.Known(), iv.Format.Value, iv.Format.Source)
				fact("architecture", iv.Architecture.Known(), iv.Architecture.Value, iv.Architecture.Source)
				fact("precision", iv.Precision.Known(), iv.Precision.Value, iv.Precision.Source)
				fact("context", iv.ContextLength.Known(), iv.ContextLength.Value, iv.ContextLength.Source)
				if len(in.Missing) > 0 {
					c.Info("  missing files: %s", strings.Join(in.Missing, ", "))
				}
				for _, p := range in.Problems {
					c.Info("  problem: %s", p)
				}
			}
			host := l.Host()
			rec := l.Registry.Recommend(&en.Model, host, ins)
			c.Info("Automatic choice: %s", map[bool]string{true: "none", false: rec.BackendID + " (" + rec.VariantID + ")"}[rec.BackendID == ""])
			// Every variant against every backend, for review.
			findings := []backends.Finding{}
			for _, v := range en.Model.Variants {
				in := ins[v.ID]
				c.Info("Variant %s:", v.ID)
				for _, d := range l.Registry.All() {
					f := backends.Assess(&en.Model, &in.Variant, d, host, in.Missing)
					findings = append(findings, f)
					codes := []string{}
					for _, r := range f.Reasons {
						codes = append(codes, r.Code)
					}
					c.Info("  %-30s %-12s %s", d.ID, f.Rating, strings.Join(codes, ","))
				}
			}
			return map[string]any{"model": en.Model, "inspections": ins, "recommendation": rec, "findings": findings,
				"host": map[string]any{"os": host.OS, "arch": host.Arch, "accelerators": host.Accelerators}}, nil
		})}
}

func modelsExportCommand() *cli.Command {
	return &cli.Command{Name: "export", Summary: "Print a model definition (no device choices, no credentials)", Usage: "<modelId>", MinArgs: 1, MaxArgs: 1,
		Run: withLibrary(func(c *cli.Ctx, e *env, l *library.Library) (any, error) {
			en, err := l.Get(context.Background(), c.Args[0])
			if err != nil {
				return nil, cli.Inputf("model %q is not in the catalog or imported", c.Args[0])
			}
			if !c.Global.JSON {
				b, _ := en.Model.Export()
				c.Info("%s", b)
			}
			return map[string]any{"model": en.Model}, nil
		})}
}

func modelsDeleteCommand() *cli.Command {
	return &cli.Command{Name: "delete", Summary: "Delete an imported model and its files", Usage: "<modelId>", MinArgs: 1, MaxArgs: 1,
		Options: []cli.Option{{Name: "yes", Alias: "y", Bool: true, Help: "Confirm"}},
		Run: withLibrary(func(c *cli.Ctx, e *env, l *library.Library) (any, error) {
			id := c.Args[0]
			en, err := l.Get(context.Background(), id)
			if err != nil || !en.Imported {
				return nil, cli.Inputf("%q is not an imported model (catalog models are removed with their files from the models directory)", id)
			}
			if !c.Bool("yes") {
				return nil, cli.Inputf("this deletes %s; re-run with --yes", en.Dir)
			}
			err = root.WithOperation(e.Root.Dir, "uninstall model", func() error {
				if _, err := e.Store.DeleteImportedModel(context.Background(), id); err != nil {
					return err
				}
				return os.RemoveAll(en.Dir)
			})
			if err != nil {
				return nil, err
			}
			c.Info("Deleted %s.", id)
			return map[string]any{"deleted": id}, nil
		})}
}

func backendsCommand() *cli.Command {
	return &cli.Command{Name: "backends", Summary: "List and register model backends", Sub: []*cli.Command{
		{Name: "list", Summary: "List registered backends and their state on this device", Run: withLibrary(func(c *cli.Ctx, e *env, l *library.Library) (any, error) {
			out := []map[string]any{}
			for _, d := range l.Registry.All() {
				kind := "built-in"
				if !d.BuiltIn {
					kind = "custom"
				}
				h := l.Host()
				c.Info("%-30s %-10s %-40s %s", d.ID, kind, h.InstallLabel(d.ID), strings.Join(d.Tasks, ","))
				out = append(out, map[string]any{"id": d.ID, "name": d.Name, "engine": d.Engine, "builtIn": d.BuiltIn,
					"install": h.Install[d.ID], "installLabel": h.InstallLabel(d.ID), "automatic": d.Automatic == nil || *d.Automatic, "automaticTasks": d.AutomaticTasks,
					"tasks": d.Tasks, "adapter": d.Adapter})
			}
			for _, d := range l.Registry.Skipped() {
				c.Info("Skipped: %s", d)
			}
			c.Info("Accelerators on this device: %s", strings.Join(l.Host().Accelerators, ", "))
			return map[string]any{"backends": out, "skipped": l.Registry.Skipped(), "host": l.Host()}, nil
		})},
		{Name: "show", Summary: "Show a backend definition", Usage: "<backendId>", MinArgs: 1, MaxArgs: 1,
			Run: withLibrary(func(c *cli.Ctx, e *env, l *library.Library) (any, error) {
				d := l.Registry.Get(c.Args[0])
				if d == nil {
					return nil, cli.Inputf("backend %q is not registered", c.Args[0])
				}
				b, _ := json.MarshalIndent(d, "", "  ")
				if !c.Global.JSON {
					c.Info("%s", b)
				}
				return map[string]any{"backend": d, "builtIn": d.BuiltIn, "install": l.Host().Install[d.ID]}, nil
			})},
		{Name: "add", Summary: "Register a custom backend from a definition file", Options: []cli.Option{
			{Name: "file", Value: "<definition.json>", Help: "A localbase.backend/v1 definition"},
		}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			path, err := required(c, "file", "file")
			if err != nil {
				return nil, err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return nil, cli.Inputf("unable to read %s", path)
			}
			d := &backends.Definition{}
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(d); err != nil {
				return nil, cli.Inputf("invalid backend definition: %v", err)
			}
			if err := root.WithOperation(e.Root.Dir, "register backend", func() error { return backends.Add(e.Root.Dir, d) }); err != nil {
				return nil, cli.Inputf("%v", err)
			}
			c.Info("Registered backend %s. LocalBase starts %s only when a model you chose it for runs.", d.ID, d.Launch.Command)
			return map[string]any{"backend": d}, nil
		})},
		{Name: "remove", Summary: "Unregister a custom backend", Usage: "<backendId>", MinArgs: 1, MaxArgs: 1,
			Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				var removed bool
				err := root.WithOperation(e.Root.Dir, "register backend", func() (err error) {
					removed, err = backends.Remove(e.Root.Dir, c.Args[0])
					return err
				})
				if err != nil {
					return nil, err
				}
				if removed {
					c.Info("Removed %s. Models set to it keep that choice and show it as unavailable.", c.Args[0])
				} else {
					c.Info("%s is not a registered custom backend.", c.Args[0])
				}
				return map[string]any{"removed": removed}, nil
			})},
	}}
}

// authProfilesCommand manages private download headers. Values come from
// environment variables or stdin, never from the command line, so they stay
// out of shell history and process listings.
func authProfilesCommand() *cli.Command {
	return &cli.Command{Name: "auth-profiles", Summary: "Manage private download headers", Sub: []*cli.Command{
		{Name: "list", Summary: "List auth profiles (header names only)", Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			ps, err := acquire.LoadProfiles(e.Root.Dir)
			if err != nil {
				return nil, err
			}
			out := []acquire.PublicProfile{}
			for _, p := range ps.Profiles {
				pub := p.Public()
				c.Info("%s: %s -> %s", pub.ID, strings.Join(pub.HeaderNames, ", "), strings.Join(pub.Origins, ", "))
				out = append(out, pub)
			}
			return map[string]any{"profiles": out}, nil
		})},
		{Name: "add", Summary: "Add or replace an auth profile", Options: []cli.Option{
			{Name: "id", Value: "<id>", Help: "Profile id"},
			{Name: "origin", Value: "<https://host>", Repeatable: true, Help: "An origin the headers may be sent to (repeatable)"},
			{Name: "header-env", Value: "<Name=ENV_VAR>", Repeatable: true, Help: "A header whose value is read from an environment variable"},
			{Name: "header-stdin", Value: "<Name>", Help: "A header whose value is read from the first line of stdin"},
		}, Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			id, err := required(c, "id", "id")
			if err != nil {
				return nil, err
			}
			p := acquire.Profile{ID: id, Origins: c.Strings("origin"), Headers: map[string]string{}}
			for _, spec := range c.Strings("header-env") {
				name, env, ok := strings.Cut(spec, "=")
				if !ok || name == "" || env == "" {
					return nil, cli.Inputf("--header-env takes Name=ENV_VAR")
				}
				v := os.Getenv(env)
				if v == "" {
					return nil, cli.Inputf("environment variable %s is empty or unset", env)
				}
				p.Headers[name] = v
			}
			if name := c.String("header-stdin"); name != "" {
				line, err := bufio.NewReader(io.LimitReader(os.Stdin, 32<<10)).ReadString('\n')
				line = strings.TrimRight(line, "\r\n")
				if (err != nil && err != io.EOF) || line == "" {
					return nil, cli.Inputf("no header value on stdin")
				}
				p.Headers[name] = line
			}
			if err := acquire.SaveProfile(e.Root.Dir, p); err != nil {
				return nil, cli.Inputf("%v", err)
			}
			pub := p.Public()
			c.Info("Saved auth profile %s (%s) for %s.", pub.ID, strings.Join(pub.HeaderNames, ", "), strings.Join(pub.Origins, ", "))
			return map[string]any{"profile": pub}, nil
		})},
		{Name: "remove", Summary: "Remove an auth profile", Usage: "<id>", MinArgs: 1, MaxArgs: 1,
			Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
				removed, err := acquire.RemoveProfile(e.Root.Dir, c.Args[0])
				if err != nil {
					return nil, err
				}
				return map[string]any{"removed": removed}, nil
			})},
	}}
}

// modelsAddDefinitionCommand adds a model definition exported elsewhere
// (models export). Only the definition travels: its files are downloaded
// separately, and backend choices are never part of it, so the model starts
// in automatic mode on this device.
func modelsAddDefinitionCommand() *cli.Command {
	return &cli.Command{Name: "add-definition", Summary: "Add a model definition exported by models export", Usage: "<file.json>", MinArgs: 1, MaxArgs: 1,
		Run: withLibrary(func(c *cli.Ctx, e *env, l *library.Library) (any, error) {
			b, err := os.ReadFile(c.Args[0])
			if err != nil {
				return nil, cli.Inputf("unable to read %s", c.Args[0])
			}
			var wrapped struct {
				Model *models.Model `json:"model"`
				Data  *struct {
					Model *models.Model `json:"model"`
				} `json:"data"`
			}
			m := &models.Model{}
			if json.Unmarshal(b, &wrapped) == nil && wrapped.Data != nil && wrapped.Data.Model != nil {
				m = wrapped.Data.Model
			} else if wrapped.Model != nil {
				m = wrapped.Model
			} else if err := json.Unmarshal(b, m); err != nil {
				return nil, cli.Inputf("not a model definition: %v", err)
			}
			if err := m.Validate(); err != nil {
				return nil, cli.Inputf("invalid model definition: %v", err)
			}
			if _, err := l.Get(context.Background(), m.ID); err == nil {
				return nil, cli.Inputf("a model with id %s already exists here", m.ID)
			}
			m.Origin = "import"
			def, _ := json.Marshal(m)
			if err := e.Store.SaveImportedModel(context.Background(), m.ID, def); err != nil {
				return nil, err
			}
			en, err := l.Get(context.Background(), m.ID)
			if err != nil {
				return nil, err
			}
			sel, err := l.Selection(context.Background(), en)
			if err != nil {
				return nil, err
			}
			c.Info("Added %s. Its files are not downloaded; backend selection on this device is automatic.", m.ID)
			printSelection(c, sel, l.Registry)
			return map[string]any{"model": m, "selection": sel}, nil
		})}
}
