package memory

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Topology describes the memory pools admission reasons about. The skeleton
// models unified memory (Apple silicon) and a host-only pool (Linux without a
// monitored GPU); discrete NVIDIA pools arrive with Linux qualification.
type Topology struct {
	Unified            bool
	SystemCapacity     int64
	GPUWorkingSetBytes int64 // unified only; 0 = no cap known
}

// Snapshot is one reading of available memory. Available < 0 = unavailable.
type Snapshot struct {
	Available int64
	Pressure  string // normal | constrained | critical | unknown
}

// HostProvider reads host memory. Tests use a fake.
type HostProvider interface {
	Topology() Topology
	Snapshot(ctx context.Context) Snapshot
}

// NewHostProvider returns the provider for this OS.
func NewHostProvider() HostProvider {
	if runtime.GOOS == "darwin" {
		return newDarwinProvider()
	}
	return &linuxProvider{capacity: linuxMemTotal()}
}

// --- darwin ---------------------------------------------------------------

// darwinProvider reads free + speculative + inactive pages with vm_stat, the same figure
// the TS provider reads through host_statistics64 (see DESIGN.md §10 on
// replacing the exec with purego).
type darwinProvider struct {
	topo Topology
}

const gpuWorkingSetEnv = "LOCALBASE_GPU_WORKING_SET_GB"

func newDarwinProvider() *darwinProvider {
	total := sysctlInt("hw.memsize")
	ws := int64(0)
	if raw := strings.TrimSpace(os.Getenv(gpuWorkingSetEnv)); raw != "" {
		if gb, err := strconv.ParseFloat(raw, 64); err == nil && gb > 0 {
			ws = int64(gb * float64(GiB))
		}
	}
	if ws == 0 {
		ws = appleWorkingSetHeuristic(total)
	}
	return &darwinProvider{topo: Topology{Unified: true, SystemCapacity: total, GPUWorkingSetBytes: ws}}
}

// appleWorkingSetHeuristic: ~2/3 of memory up to 36 GiB, ~3/4 above.
func appleWorkingSetHeuristic(total int64) int64 {
	if total <= 0 {
		return 0
	}
	if total <= 36*GiB {
		return total * 2 / 3
	}
	return total * 3 / 4
}

func (d *darwinProvider) Topology() Topology { return d.topo }

var vmStatRe = regexp.MustCompile(`page size of (\d+) bytes`)

func (d *darwinProvider) Snapshot(ctx context.Context) Snapshot {
	out, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
	if err != nil {
		return Snapshot{Available: -1, Pressure: "unknown"}
	}
	avail, err := parseVMStat(out)
	if err != nil {
		return Snapshot{Available: -1, Pressure: "unknown"}
	}
	return Snapshot{Available: avail, Pressure: "normal"}
}

// parseVMStat returns (free + speculative + inactive) * page size: TS reads
// free_count + inactive_count from host_statistics64, and vm_stat prints
// "Pages free" as free_count - speculative_count, with speculative apart.
func parseVMStat(out []byte) (int64, error) {
	m := vmStatRe.FindSubmatch(out)
	if m == nil {
		return 0, errors.New("vm_stat: no page size")
	}
	page, _ := strconv.ParseInt(string(m[1]), 10, 64)
	var free, inactive, speculative int64 = -1, -1, 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Pages free":
			free = n
		case "Pages inactive":
			inactive = n
		case "Pages speculative":
			speculative = n
		}
	}
	if free < 0 || inactive < 0 || page <= 0 {
		return 0, errors.New("vm_stat: missing counters")
	}
	return (free + speculative + inactive) * page, nil
}

func sysctlInt(name string) int64 {
	out, err := exec.Command("/usr/sbin/sysctl", "-n", name).Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return n
}

// --- linux ----------------------------------------------------------------

type linuxProvider struct{ capacity int64 }

func (l *linuxProvider) Topology() Topology { return Topology{SystemCapacity: l.capacity} }

func (l *linuxProvider) Snapshot(context.Context) Snapshot {
	if v := meminfo("MemAvailable"); v >= 0 {
		return Snapshot{Available: v, Pressure: "normal"}
	}
	return Snapshot{Available: -1, Pressure: "unknown"}
}

func linuxMemTotal() int64 { return max(0, meminfo("MemTotal")) }

func meminfo(key string) int64 {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && k == key {
			f := strings.Fields(v)
			if len(f) > 0 {
				n, _ := strconv.ParseInt(f[0], 10, 64)
				return n * 1024
			}
		}
	}
	return -1
}

// Footprint samples a process's physical footprint (macOS `footprint`
// TOTAL), which includes Metal allocations. Used to cap switch-preflight
// release credit (#355) and to record observed peaks.
func Footprint(ctx context.Context, pid int) (int64, bool) {
	if runtime.GOOS != "darwin" || pid <= 0 {
		return 0, false
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/footprint", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	return parseFootprint(out)
}

var sizeRe2 = regexp.MustCompile(`(?i)([\d.]+)\s*(B|KB|MB|GB|TB)\b`)
var totalLineRe = regexp.MustCompile(`\bTOTAL\s*$`)

// parseFootprint sums dirty + clean from the TOTAL row, like the TS parser:
// phys_footprint alone omits clean mapped weights that are resident while a
// model serves.
func parseFootprint(out []byte) (int64, bool) {
	units := map[string]float64{"B": 1, "KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40}
	for _, line := range strings.Split(string(out), "\n") {
		if !totalLineRe.MatchString(line) {
			continue
		}
		m := sizeRe2.FindAllStringSubmatch(line, -1)
		if len(m) < 2 {
			return 0, false
		}
		var total float64
		for _, s := range m[:2] {
			v, err := strconv.ParseFloat(s[1], 64)
			if err != nil {
				return 0, false
			}
			total += v * units[strings.ToUpper(s[2])]
		}
		return int64(total + 0.5), true
	}
	return 0, false
}
