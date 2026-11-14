package harness

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The helpers below are used by the tests of more than one suite package (go/test/contract and
// go/test/system), so they live here rather than in either (step 7, R3).

// DiffJSON returns the paths where a and b differ, skipping paths for which
// skip returns true.
func DiffJSON(path string, a, b any, skip func(string) bool) []string {
	if skip != nil && skip(path) {
		return nil
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			return []string{path}
		}
		var out []string
		keys := map[string]bool{}
		for k := range av {
			keys[k] = true
		}
		for k := range bv {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			out = append(out, DiffJSON(path+"."+k, av[k], bv[k], skip)...)
		}
		return out
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return []string{path + fmt.Sprintf(" (len %d vs %v)", len(av), len(bv))}
		}
		var out []string
		for i := range av {
			out = append(out, DiffJSON(fmt.Sprintf("%s[%d]", path, i), av[i], bv[i], skip)...)
		}
		return out
	}
	if !reflect.DeepEqual(a, b) {
		return []string{fmt.Sprintf("%s: %v vs %v", path, a, b)}
	}
	return nil
}

// EntryOf is the model-management state's entry for id; it fails the test when there is none.
func EntryOf(t *testing.T, state map[string]any, id string) map[string]any {
	t.Helper()
	for _, m := range state["models"].([]any) {
		if e := m.(map[string]any); e["id"] == id {
			return e
		}
	}
	t.Fatalf("%s not listed", id)
	return nil
}

// LogFreeMemoryOnFailure makes a failing admission test diagnosable: memory
// refusals depend on what else is loaded on the machine.
func LogFreeMemoryOnFailure(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("free memory at failure: %s", FreeMemoryReading())
		}
	})
}

// FreeMemoryReading is the operating system's own figure: memory_pressure on
// macOS, MemAvailable on Linux.
func FreeMemoryReading() string {
	if out, err := exec.Command("memory_pressure").Output(); err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return strings.TrimSpace(lines[len(lines)-1])
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemAvailable:") {
				return strings.Join(strings.Fields(line), " ")
			}
		}
	}
	return "unavailable"
}

// VoiceClip is a 4 s 16 kHz PCM16 WAV of deterministic noise.
func VoiceClip(seed uint32) []byte {
	const frames = 64000
	b := make([]byte, 44, 44+frames*2)
	copy(b, "RIFF")
	copy(b[8:], "WAVEfmt ")
	le := binary.LittleEndian
	le.PutUint32(b[16:], 16)
	le.PutUint16(b[20:], 1)
	le.PutUint16(b[22:], 1)
	le.PutUint32(b[24:], 16000)
	le.PutUint32(b[28:], 32000)
	le.PutUint16(b[32:], 2)
	le.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	le.PutUint32(b[40:], frames*2)
	x := seed
	for i := 0; i < frames*2; i++ {
		x = x*1664525 + 1013904223
		b = append(b, byte(x>>24))
	}
	le.PutUint32(b[4:], uint32(len(b)-8))
	return b
}
