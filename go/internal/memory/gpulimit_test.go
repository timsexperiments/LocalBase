package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseListDevices(t *testing.T) {
	const real = "Available devices:\n  MTL0: Apple M4 Max (53084 MiB, 53083 MiB free)\n  BLAS: Accelerate (0 MiB, 0 MiB free)\n"
	if v, ok := parseListDevices([]byte(real)); !ok || v != 53084*MiB {
		t.Errorf("this Mac's real output: %d %v", v, ok)
	}
	for name, out := range map[string]string{
		"no devices":  "Available devices:\n",
		"BLAS only":   "Available devices:\n  BLAS: Accelerate (0 MiB, 0 MiB free)\n",
		"NVIDIA":      "Available devices:\n  CUDA0: NVIDIA (24000 MiB, 23000 MiB free)\n",
		"zero":        "  MTL0: Apple (0 MiB, 0 MiB free)\n",
		"overflowing": "  MTL0: Apple (99999999999999999999 MiB, 0 MiB free)\n",
		"not it":      "error: MTL0 is unavailable\n",
		"empty":       "",
	} {
		if v, ok := parseListDevices([]byte(out)); ok {
			t.Errorf("%s: %d", name, v)
		}
	}
}

// fakeServer is a llama-server that only knows --list-devices.
func fakeServer(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "llama-server")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func macProvider(pinned bool) *darwinProvider {
	total := 64 * GiB
	return &darwinProvider{topo: Topology{Unified: true, SystemCapacity: total, GPUWorkingSetBytes: appleWorkingSetHeuristic(total)}, pinned: pinned}
}

const metalLine = `echo "  MTL0: Apple M4 Max (53084 MiB, 53083 MiB free)"`

func TestProbeGPUWorkingSet(t *testing.T) {
	ctx := context.Background()
	d := macProvider(false)
	if got := d.Topology().GPUWorkingSetBytes; got != 48*GiB {
		t.Fatalf("the heuristic: %d", got)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	bin := fakeServer(t, `[ "$1" = "--list-devices" ] || exit 9
echo x >> "`+marker+`"
`+metalLine)
	if v, ok := ProbeGPUWorkingSet(ctx, d, bin); !ok || v != 53084*MiB {
		t.Fatalf("probe: %d %v", v, ok)
	}
	if got := d.Topology().GPUWorkingSetBytes; got != 53084*MiB {
		t.Errorf("the limit: %d", got)
	}
	// Read once: a second call spawns nothing.
	ProbeGPUWorkingSet(ctx, d, bin)
	if b, _ := os.ReadFile(marker); string(b) != "x\n" {
		t.Errorf("ran more than once: %q", b)
	}
}

func TestProbeGPUWorkingSetFallbacks(t *testing.T) {
	ctx := context.Background()
	for name, script := range map[string]string{
		"fails":       "exit 1",
		"no Metal":    `echo "  BLAS: Accelerate (0 MiB, 0 MiB free)"`,
		"past memory": `echo "  MTL0: Apple (999999 MiB, 0 MiB free)"`,
		"garbage":     "echo nonsense",
	} {
		d := macProvider(false)
		if _, ok := ProbeGPUWorkingSet(ctx, d, fakeServer(t, script)); ok {
			t.Errorf("%s: accepted", name)
		}
		if got := d.Topology().GPUWorkingSetBytes; got != 48*GiB {
			t.Errorf("%s: the heuristic should stand: %d", name, got)
		}
	}
	d := macProvider(false)
	if _, ok := ProbeGPUWorkingSet(ctx, d, filepath.Join(t.TempDir(), "missing")); ok {
		t.Error("a missing binary was accepted")
	}
	// A hung binary cannot hold startup: the context ends it.
	short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, ok := ProbeGPUWorkingSet(short, d, fakeServer(t, "exec sleep 30")); ok || time.Since(start) > 5*time.Second {
		t.Errorf("hung: %v after %v", ok, time.Since(start))
	}
	// A failed probe can succeed later, once a runtime is installed.
	if _, ok := ProbeGPUWorkingSet(ctx, d, fakeServer(t, metalLine)); !ok {
		t.Error("a later probe should work")
	}
	// The environment override wins.
	pinned := macProvider(true)
	pinned.topo.GPUWorkingSetBytes = 20 * GiB
	if _, ok := ProbeGPUWorkingSet(ctx, pinned, fakeServer(t, metalLine)); ok || pinned.Topology().GPUWorkingSetBytes != 20*GiB {
		t.Errorf("the override: %d", pinned.Topology().GPUWorkingSetBytes)
	}
	// Not a Mac: nothing happens.
	if _, ok := ProbeGPUWorkingSet(ctx, &tableLimitHost{}, fakeServer(t, metalLine)); ok {
		t.Error("another provider was probed")
	}
}

type tableLimitHost struct{}

func (*tableLimitHost) Topology() Topology { return Topology{SystemCapacity: 64 * GiB} }
func (*tableLimitHost) Snapshot(context.Context) Snapshot {
	return Snapshot{Available: 60 * GiB, Pressure: "normal"}
}

// The difference admission sees: a 50 GiB model on 64 GiB, refused for good under
// the heuristic and admitted under this Mac's real limit.
func TestGPULimitReachesAdmission(t *testing.T) {
	ctx := context.Background()
	d := macProvider(false)
	host := &probedHost{d}
	c := NewController(unsampled{host}, Reserve{Percent: 15, MinimumGB: 8}, false)
	model := Demand{UnifiedBytes: 50 * GiB, AcceleratorBytes: 50 * GiB, Confidence: "derived"}
	var ae *AdmissionError
	if err := c.CheckAdmission(ctx, model, nil); !errors.As(err, &ae) || !ae.Permanent || ae.PoolID != "gpu-working-set" {
		t.Fatalf("under the heuristic: %v", err)
	}
	if _, ok := ProbeGPUWorkingSet(ctx, d, fakeServer(t, metalLine)); !ok {
		t.Fatal("probe")
	}
	if err := c.CheckAdmission(ctx, model, nil); errors.As(err, &ae) && ae.Permanent {
		t.Errorf("under the real limit it should not be permanent: %v", err)
	}
}

// probedHost is a darwinProvider's topology with a fixed reading.
type probedHost struct{ *darwinProvider }

func (*probedHost) Snapshot(context.Context) Snapshot {
	return Snapshot{Available: 62 * GiB, Pressure: "normal"}
}

// Asking the backend's program about its devices keeps the gateway's credentials back.
func TestProbingDevicesKeepsCredentialsBack(t *testing.T) {
	dir := t.TempDir()
	bin, out := filepath.Join(dir, "dump"), filepath.Join(dir, "env.txt")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n/usr/bin/env > \"$ENV_DUMP\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENV_DUMP", out)
	t.Setenv("HF_TOKEN", "planted-token")
	ProbeGPUWorkingSet(context.Background(), &darwinProvider{topo: Topology{Unified: true, SystemCapacity: 64 * GiB}}, bin)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the program didn't run: %v", err)
	}
	if strings.Contains(string(b), "planted-") || !strings.Contains(string(b), "ENV_DUMP=") {
		t.Errorf("got: %s", b)
	}
}
