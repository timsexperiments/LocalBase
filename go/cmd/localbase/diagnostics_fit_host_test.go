package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// sizedHost is a computer with exactly gib GiB, all of it free.
type sizedHost struct{ gib int64 }

func (h sizedHost) Topology() memory.Topology {
	return memory.Topology{Unified: true, SystemCapacity: h.gib * memory.GiB, GPUWorkingSetBytes: h.gib * memory.GiB}
}

func (h sizedHost) Snapshot(context.Context) memory.Snapshot {
	return memory.Snapshot{Available: h.gib * memory.GiB, Pressure: "normal"}
}

// The bundle judges the models' fit against the command's host, as models list and show do, not
// against what memory.DetectSpecs reads from the machine (0 GB on Linux and Intel Macs, so every
// model "wouldn't fit"). Two hosts no machine is both of, 1 GiB and 512 GiB, give the bundle two
// different figures and verdicts, whatever this computer has.
func TestDiagnosticsJudgesFitOnTheCommandsHost(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("LOCALBASE_ROOT", "")
	root := filepath.Join(t.TempDir(), "root")
	if code, out := runApp(t, "--root", root, "configure", "--defaults", "--no-create-key"); code != 0 {
		t.Fatalf("configure: %s", out)
	}
	type modelMemory struct {
		ID     string `json:"id"`
		Memory struct {
			Verdict        string  `json:"verdict"`
			SystemMemoryGb float64 `json:"systemMemoryGb"`
		} `json:"memory"`
	}
	bundleOn := func(gib int64) []modelMemory {
		t.Helper()
		bundle := filepath.Join(t.TempDir(), "bundle.zip")
		a := app()
		a.Context = context.WithValue(context.Background(), testHostKey{}, memory.HostProvider(sizedHost{gib: gib}))
		if code := a.Run([]string{"--root", root, "diagnostics", "--output", bundle}); code != 0 {
			t.Fatalf("diagnostics on %d GiB: exit %d", gib, code)
		}
		zr, err := zip.OpenReader(bundle)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			if f.Name != "manifest.json" {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			var m struct {
				Models struct {
					Data struct {
						Models []modelMemory `json:"models"`
					} `json:"data"`
				} `json:"models"`
			}
			if err := json.NewDecoder(rc).Decode(&m); err != nil {
				t.Fatal(err)
			}
			if len(m.Models.Data.Models) == 0 {
				t.Fatalf("on %d GiB: the bundle lists no models", gib)
			}
			return m.Models.Data.Models
		}
		t.Fatal("no manifest")
		return nil
	}
	// On 1 GiB nothing fits comfortably (a model whose minimum is 1 GB is "tight"); on 512 GiB everything does.
	for _, c := range []struct {
		gib  int64
		fits bool
	}{{1, false}, {512, true}} {
		for _, m := range bundleOn(c.gib) {
			if m.Memory.SystemMemoryGb != float64(c.gib) || (m.Memory.Verdict == "fits") != c.fits {
				t.Errorf("on %d GiB, %s: systemMemoryGb %v, verdict %q", c.gib, m.ID, m.Memory.SystemMemoryGb, m.Memory.Verdict)
			}
		}
	}
}
