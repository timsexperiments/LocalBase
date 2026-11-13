package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/diagnostics"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/meta"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/termsafe"
)

func diagnosticsCommand() *cli.Command {
	return &cli.Command{Name: "diagnostics", Summary: "Create a redacted LocalBase diagnostics bundle",
		Examples: []string{"local-base diagnostics", "local-base diagnostics --output report.zip"},
		Options:  []cli.Option{{Name: "output", Value: "<path.zip>", Help: "Output ZIP path"}},
		Run:      runDiagnostics}
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
	b := diagnostics.Collect(context.Background(), dir, service.NewManager())
	if b.DatabaseErr != nil {
		c.Error("Could not read this root's database: %s", termsafe.Printable(b.DatabaseErr.Error()))
	}
	path, err := diagnostics.OutputPath(requested)
	if err != nil {
		return nil, err
	}
	size, err := diagnostics.WriteArchive(path, b.Entries)
	if err != nil {
		return nil, err
	}
	c.Info("Created diagnostics bundle: %s", path)
	for _, line := range b.Problems {
		c.Info("Could not read: %s", line)
	}
	if b.Unfinished != "" {
		c.Info("%s", b.Unfinished)
	}
	if stderr := b.ServiceStderr; stderr != nil {
		c.Info("Service stderr: %d bytes captured at %s. It holds unredacted gateway crash output, so the bundle records only its size; review it before sharing.",
			stderr.Bytes, stderr.Path)
	}
	if b.Models != nil {
		printDiagnosticModels(c, b.Models)
	}
	return map[string]any{"archive": map[string]any{"path": path, "entries": len(b.Entries), "bytes": size}}, nil
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
