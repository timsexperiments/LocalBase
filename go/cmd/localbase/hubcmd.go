package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/hub"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/meta"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// models search | add hf: | remove-entry (TS models/commands/hf.ts).

// hubToken is the saved token, else HF_TOKEN; sent only to the Hub.
func hubToken(cfg *store.Config) string {
	if cfg != nil && cfg.HFToken != "" {
		return cfg.HFToken
	}
	return os.Getenv("HF_TOKEN")
}

func compactCount(v int64) string {
	switch {
	case v >= 1e6:
		return jsFixed1(float64(v)/1e6) + "M"
	case v >= 1e3:
		return jsFixed1(float64(v)/1e3) + "k"
	}
	return strconv.FormatInt(v, 10)
}

func jsFixed1(x float64) string { return jsjson.ToFixed1(x) }

func modelsSearch() *cli.Command {
	return &cli.Command{Name: "search", Summary: "Search Hugging Face for models LocalBase can run", Usage: "[query...]", MaxArgs: 20,
		Examples: []string{"local-base models search qwen coder", "local-base --json models search gemma --limit 5", "local-base models search --kind stt"},
		Options: []cli.Option{
			{Name: "kind", Value: "<llm|stt|image>", Help: "What to search for: GGUF language models (default), whisper.cpp speech models or text-to-image checkpoints"},
			{Name: "limit", Value: "<count>", Help: "Number of results (default 20, at most 100)"},
		},
		Run: func(c *cli.Ctx) (any, error) {
			kind := "llm"
			if c.Has("kind") {
				kind = c.String("kind")
				if !slices.Contains(hub.AddableKinds, kind) {
					return nil, cli.Inputf(`kind: Invalid option: expected one of "llm"|"stt"|"image"`)
				}
			}
			limit := 20
			if c.Has("limit") {
				n, err := strconv.ParseFloat(strings.TrimSpace(c.String("limit")), 64)
				switch {
				case err != nil || strings.TrimSpace(c.String("limit")) == "":
					return nil, cli.Inputf("limit: Invalid input: expected number, received NaN")
				case n != float64(int(n)):
					return nil, cli.Inputf("limit: Invalid input: expected int, received number")
				case n < 1:
					return nil, cli.Inputf("limit: Too small: expected number to be >=1")
				case n > 100:
					return nil, cli.Inputf("limit: Too big: expected number to be <=100")
				}
				limit = int(n)
			}
			dir, err := root.Canonical(root.Resolve(c.Global.Root))
			if err != nil {
				return nil, err
			}
			var cfg *store.Config
			if _, err := os.Stat(filepath.Join(dir, "local-base.db")); err == nil {
				if st, err := store.Open(filepath.Join(dir, "local-base.db")); err == nil {
					cfg, _ = st.LoadConfig(context.Background())
					st.Close()
				}
			}
			query := strings.Join(c.Args, " ")
			client := &hub.Client{Root: dir, Token: hubToken(cfg)}
			hits, cached, err := client.Search(context.Background(), kind, query, limit)
			if err != nil {
				return nil, err
			}
			if len(hits) == 0 {
				switch kind {
				case "llm":
					c.Info("No GGUF models on Hugging Face match that search.")
				case "stt":
					c.Info("No speech recognition models on Hugging Face match that search.")
				default:
					c.Info("No text-to-image models on Hugging Face match that search.")
				}
			} else {
				dash := func(s *string) string {
					if s == nil {
						return "-"
					}
					return *s
				}
				rows := [][]string{{"MODEL", "DOWNLOADS", "LICENSE", "PIPELINE", "GATED", "LIKELY KIND"}}
				for _, h := range hits {
					gated := "-"
					if h.Gated {
						gated = "yes"
					}
					rows = append(rows, []string{h.Repo, compactCount(h.Downloads), dash(h.License), dash(h.PipelineTag), gated, h.Likely.Label})
				}
				widths := make([]int, len(rows[0]))
				for _, row := range rows {
					for i, cell := range row {
						widths[i] = max(widths[i], jsLength(cell))
					}
				}
				for _, row := range rows {
					var b strings.Builder
					for i, cell := range row {
						if i > 0 {
							b.WriteString("  ")
						}
						b.WriteString(cell)
						if i < len(row)-1 {
							b.WriteString(strings.Repeat(" ", widths[i]-jsLength(cell)))
						}
					}
					c.Info("%s", b.String())
				}
				c.Info("")
				c.Info("Add one with: local-base models add hf:<org>/<repo>[:<file-or-quant>]")
				if slices.ContainsFunc(hits, func(h hub.Hit) bool { return h.Gated }) {
					c.Info("Gated repositories need their terms accepted at https://huggingface.co/<org>/<repo> and a saved Hugging Face token.")
				}
			}
			if cached {
				c.Info("(Results from the local cache.)")
			}
			results := []map[string]any{}
			for _, h := range hits {
				r := map[string]any{"repo": h.Repo, "downloads": h.Downloads, "license": h.License, "pipelineTag": h.PipelineTag,
					"likelyKind": h.Likely.Label, "kind": h.Likely.Kind, "gated": h.Gated, "addable": h.Likely.Kind != nil}
				if h.Likes != nil {
					r["likes"] = *h.Likes
				}
				results = append(results, r)
			}
			return map[string]any{"query": query, "kind": kind, "cached": cached, "results": results}, nil
		}}
}

// jsLength is a string's length in UTF-16 units (padEnd counts those).
func jsLength(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// userOwned refuses ids a built-in or remote entry owns; true when a user
// entry already has the id.
func userOwned(cat *catalog.Catalog, modelID string) (bool, error) {
	s := cat.ByID(modelID)
	if s == nil {
		return false, nil
	}
	if s.Origin != "user" {
		return false, cli.Inputf("%q is already a %s catalog model. Choose another id with --id, or patch the existing entry by hand in catalog.d (see docs/user-catalog.md).", modelID, s.Origin)
	}
	return true, nil
}

func modelsAdd() *cli.Command {
	return &cli.Command{Name: "add", Summary: "Add a GGUF, whisper.cpp or Stable Diffusion model from Hugging Face to the user catalog",
		Usage: "<reference>", MinArgs: 1, MaxArgs: 1,
		Examples: []string{"local-base models add hf:Qwen/Qwen2.5-0.5B-Instruct-GGUF:q4_k_m", "local-base models add hf:ggml-org/gemma-3-4b-it-GGUF --install",
			"local-base models add hf:ggerganov/whisper.cpp:ggml-small-q8_0.bin",
			"local-base models add hf:stable-diffusion-v1-5/stable-diffusion-v1-5:v1-5-pruned-emaonly.safetensors"},
		Options: []cli.Option{
			{Name: "install", Bool: true, Help: "Download the model after adding it"},
			{Name: "id", Value: "<model-id>", Help: "Catalog id to use instead of one derived from the file name"},
		},
		Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			id := c.String("id")
			if c.Has("id") && !modelIDOK(id) {
				return nil, cli.Inputf("id: use lowercase letters, digits, '.', '_' and '-'")
			}
			ref, err := hub.ParseReference(c.Args[0])
			if err != nil {
				return nil, err
			}
			if id != "" {
				if _, err := userOwned(e.Catalog, id); err != nil {
					return nil, err
				}
			}
			client := &hub.Client{Root: e.Root.Dir, Token: hubToken(e.Config)}
			info, err := client.ModelInfo(context.Background(), ref.Repo)
			if err != nil {
				return nil, err
			}
			entry, err := hub.GenerateEntry(e.Catalog, info, ref, id)
			if err != nil {
				return nil, err
			}
			replaced, err := userOwned(e.Catalog, entry.ModelID)
			if err != nil {
				return nil, err
			}
			path, model, err := catalog.WriteUserEntry(e.Root.Dir, entry.Spec)
			if err != nil {
				return nil, err
			}
			verb := "Added"
			if replaced {
				verb = "Updated"
			}
			c.Info("%s %s (%s)", verb, model.ModelID, model.DisplayName)
			c.Info("  From:          %s at %s", entry.Repo, entry.Revision)
			for _, f := range entry.Weights {
				size := f.Size
				c.Info("  File:          %s (%s, sha256 %s…)", f.Path, meta.FormatFileSize(&size), f.SHA256[:12])
			}
			if entry.Projector != nil {
				c.Info("  Projector:     %s (image input)", entry.Projector.Path)
			}
			license := "not stated"
			if entry.License != nil {
				license = *entry.License
			}
			c.Info("  License:       %s (%s)", license, model.CommercialStatus)
			pipeline := "not stated"
			if entry.PipelineTag != nil {
				pipeline = *entry.PipelineTag
			}
			c.Info("  Pipeline:      %s", pipeline)
			if entry.Kind == "llm" {
				ctxText := "unknown until installed"
				if entry.ContextLength != nil {
					ctxText = meta.Thousands(int(*entry.ContextLength)) + " tokens"
				}
				c.Info("  Context:       %s", ctxText)
			}
			var declared, labels []string
			for _, cp := range model.Capabilities {
				declared = append(declared, cp.ID)
				labels = append(labels, meta.CapabilityLabel(cp.ID))
			}
			c.Info("  Capabilities:  %s (declared)", strings.Join(labels, ", "))
			if len(entry.Detected) > 0 {
				var det []string
				for _, d := range entry.Detected {
					det = append(det, meta.CapabilityLabel(d))
				}
				c.Info("                 %s (detected from the chat template; confirmed after install)", strings.Join(det, ", "))
			}
			c.Info("  Catalog:       User entry, not qualified by LocalBase (%s)", path)
			var installed any
			if install, _ := c.OptBool("install"); install {
				if next, err := catalog.LoadRoot(e.Root.Dir, nil); err == nil {
					e.Catalog.Replace(next)
				}
				p, err := installModel(context.Background(), e, model.ModelID, progressReporter(c))
				if err != nil {
					return nil, err
				}
				installed = p
				c.Info("Installed: %s", p)
			} else {
				c.Info("Install it with: local-base models install %s", model.ModelID)
			}
			files := []map[string]any{}
			for _, f := range entry.Weights {
				files = append(files, map[string]any{"path": f.Path, "sizeBytes": f.Size, "sha256": f.SHA256})
			}
			var projector any
			if entry.Projector != nil {
				projector = entry.Projector.Path
			}
			var ctxLen any
			if entry.ContextLength != nil {
				ctxLen = *entry.ContextLength
			}
			if declared == nil {
				declared = []string{}
			}
			return map[string]any{"modelId": model.ModelID, "path": path, "repo": entry.Repo, "revision": entry.Revision,
				"license": entry.License, "pipelineTag": entry.PipelineTag, "files": files, "projector": projector,
				"declaredCapabilities": declared, "detectedCapabilities": entry.Detected, "contextWindowTokens": ctxLen,
				"replaced": replaced, "installedPath": installed}, nil
		})}
}

func modelIDOK(id string) bool {
	if id == "" {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

func modelsRemoveEntry() *cli.Command {
	return &cli.Command{Name: "remove-entry", Summary: "Remove a model's user catalog file (downloaded files stay)", Usage: "<modelId>",
		MinArgs: 1, MaxArgs: 1, Examples: []string{"local-base models remove-entry qwen2.5-0.5b-instruct-q4_k_m"},
		Run: withEnv(func(c *cli.Ctx, e *env) (any, error) {
			id := c.Args[0]
			s := e.Catalog.ByID(id)
			selected := false
			for _, ids := range e.Config.Selected {
				selected = selected || slices.Contains(ids, id)
			}
			// Removing the only entry for a selected model would leave a
			// configuration naming a model the catalog no longer has.
			if selected && s != nil && s.Origin == "user" {
				return nil, cli.Inputf("%q is selected. Deselect it first with `local-base configure`, then remove the entry.", id)
			}
			path, err := catalog.RemoveUserEntry(e.Root.Dir, e.Catalog, id)
			if err != nil {
				return nil, err
			}
			if path == "" {
				if s != nil {
					return nil, cli.Inputf("%q is a %s catalog model with no user file; there is nothing to remove.", id, s.Origin)
				}
				return nil, cli.Inputf("No user catalog entry named %q.", id)
			}
			c.Info("Removed %s", path)
			next, err := catalog.LoadRoot(e.Root.Dir, nil)
			if err != nil {
				return nil, err
			}
			var restored any
			if r := next.ByID(id); r != nil {
				restored = r.Tier
				c.Info("%s now uses the %s catalog entry again.", id, r.Tier)
			} else {
				c.Info("Downloaded model files were left in place in the models directory; delete them by hand to free the space.")
			}
			return map[string]any{"modelId": id, "path": path, "restoredTier": restored, "selected": selected}, nil
		})}
}
