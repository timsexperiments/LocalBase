package models

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

// watchHeap ends the test binary if the heap passes limit, so that a regression
// in these tests fails in seconds instead of filling the machine's memory.
func watchHeap(t *testing.T, limit uint64) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > limit {
				fmt.Fprintf(os.Stderr, "%s: heap reached %d MiB\n", t.Name(), m.HeapAlloc>>20)
				os.Exit(2)
			}
		}
	}()
}

func splitHeader(t *testing.T, dir, name string, kv map[string]any) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := testutil.GGUF(path, kv, 0); err != nil {
		t.Fatal(err)
	}
	return path
}

func variantOf(names ...string) Variant {
	v := Variant{ID: "v"}
	for _, n := range names {
		v.Artifacts = append(v.Artifacts, Artifact{Filename: n, Role: RoleWeights, Required: true})
	}
	return v
}

func TestImplausibleSplitsAreRefused(t *testing.T) {
	watchHeap(t, 512<<20)
	for _, c := range []struct {
		name  string
		name2 string
		kv    map[string]any
		want  string
	}{
		{name: "huge count", name2: "m-00001-of-00002.gguf", kv: map[string]any{"split.count": uint64(1_000_000_000_000)}, want: "split.count"},
		{name: "huge count, one part by name", name2: "m-00001-of-00001.gguf", kv: map[string]any{"split.count": uint64(1_000_000_000_000)}, want: "split.count"},
		{name: "huge count, no part in the name", name2: "m.gguf", kv: map[string]any{"split.count": uint64(1_000_000_000_000)}, want: "split.count"},
		{name: "just over the limit", name2: "m.gguf", kv: map[string]any{"split.count": uint64(maxSplitCount + 1)}, want: "split.count"},
		{name: "not a number", name2: "m.gguf", kv: map[string]any{"split.count": "many"}, want: "split.count"},
		{name: "past what a number can say", name2: "m.gguf", kv: map[string]any{"split.count": uint64(1 << 62)}, want: "split.count"},
		{name: "contradicts the name", name2: "m-00001-of-00002.gguf", kv: map[string]any{"split.count": uint64(3)}, want: "file name"},
		{name: "index past the count", name2: "m-00001-of-00002.gguf", kv: map[string]any{"split.count": uint64(2), "split.no": uint64(2)}, want: "split.no"},
		{name: "index contradicts the name", name2: "m-00001-of-00002.gguf", kv: map[string]any{"split.count": uint64(2), "split.no": uint64(1)}, want: "split.no"},
		{name: "name claims too many parts", name2: "m-00001-of-99999.gguf", kv: map[string]any{}, want: "file name"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := splitHeader(t, dir, c.name2, c.kv)
			ff := InspectFile(path)
			if !ff.Refused || !strings.Contains(ff.Problem, c.want) || ff.SplitCount != 0 {
				t.Fatalf("%+v, want refused because of %s", ff, c.want)
			}
			in := InspectVariant(variantOf(c.name2), dir)
			if len(in.Variant.Artifacts) != 1 || len(in.Problems) != 1 {
				t.Fatalf("artifacts %d, problems %v", len(in.Variant.Artifacts), in.Problems)
			}
		})
	}
}

// Real split sets keep working, by header and by name alone.
func TestRealSplitsStillExpand(t *testing.T) {
	dir := t.TempDir()
	splitHeader(t, dir, "m-00001-of-00003.gguf", map[string]any{"split.count": uint64(3), "split.no": uint64(0)})
	splitHeader(t, dir, "m-00002-of-00003.gguf", map[string]any{"split.count": uint64(3), "split.no": uint64(1)})
	in := InspectVariant(variantOf("m-00001-of-00003.gguf"), dir)
	if len(in.Variant.Artifacts) != 3 || len(in.Missing) != 1 || in.Missing[0] != "m-00003-of-00003.gguf" {
		t.Fatalf("artifacts %+v, missing %v, problems %v", in.Variant.Artifacts, in.Missing, in.Problems)
	}
	dir = t.TempDir()
	splitHeader(t, dir, "n-00001-of-00002.gguf", map[string]any{})
	in = InspectVariant(variantOf("n-00001-of-00002.gguf"), dir)
	if len(in.Variant.Artifacts) != 2 || len(in.Missing) != 1 {
		t.Fatalf("by name: artifacts %d, missing %v", len(in.Variant.Artifacts), in.Missing)
	}
}

// The largest set a file may claim stays cheap to inspect: every part is a stat.
func TestLargestAllowedSplit(t *testing.T) {
	watchHeap(t, 512<<20)
	dir := t.TempDir()
	name := fmt.Sprintf("m-00001-of-%05d.gguf", maxSplitCount)
	splitHeader(t, dir, name, map[string]any{"split.count": uint64(maxSplitCount)})
	start := time.Now()
	in := InspectVariant(variantOf(name), dir)
	t.Logf("%d parts inspected in %v", maxSplitCount, time.Since(start))
	if len(in.Variant.Artifacts) != maxSplitCount || len(in.Missing) != maxSplitCount-1 {
		t.Fatalf("artifacts %d, missing %d", len(in.Variant.Artifacts), len(in.Missing))
	}
}

// A sidecar is read up to its limit, however large the file is.
func TestSidecarsAreReadWithinTheirLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(256 << 20); err != nil { // sparse
		t.Fatal(err)
	}
	f.Close()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if got := sidecarArchitecture(dir); got != "" {
		t.Errorf("architecture %q from an oversized sidecar", got)
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 {
		t.Errorf("allocated %d MiB reading a sidecar", grew>>20)
	}
}

// A nested header is refused here too, not merely unreadable.
func TestNestedHeaderIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.gguf")
	var b bytes.Buffer
	put := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	put(uint32(ggufMagic))
	put(uint32(3))
	put(uint64(0))
	put(uint64(1))
	put(uint64(1))
	b.WriteString("k")
	put(uint32(9))
	put(uint32(9))
	put(uint64(1))
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if ff := InspectFile(path); !ff.Refused {
		t.Fatalf("%+v, want refused", ff)
	}
}
