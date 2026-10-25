package placement

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The startup placement check (DESIGN §20.5, §20.8 item 4): llama-server's
// own output is compared with the plan, and a difference fails the start.
// The output is read as bytes; it is never decoded as UTF-8.

var (
	overrideRe  = regexp.MustCompile(`tensor (\S+) \([^\n]*?\) buffer type overridden to (\S+)`)
	offloadedRe = regexp.MustCompile(`offloaded (\d+)/(\d+) layers to GPU`)
	computeRe   = regexp.MustCompile(`CUDA0 compute buffer size =\s*([0-9.]+) MiB`)
)

// Allowed over the planned workspace: 10% or 64 MiB, whichever is larger.
const workspaceSlack = 64 << 20

func mismatchf(format string, a ...any) error {
	return &Error{"placement_mismatch", "The model's placement on this device differs from the plan, so it was stopped: " + fmt.Sprintf(format, a...)}
}

// Check validates a backend's startup output against its plan.
func Check(log []byte, p Plan) error {
	if p.Platform == Unified && len(p.Args) == 0 {
		return nil // nothing was placed
	}
	// Layers: the last offload report is the real load.
	if all := offloadedRe.FindAllSubmatch(log, -1); len(all) > 0 {
		last := all[len(all)-1]
		got, _ := strconv.Atoi(string(last[1]))
		if got != p.GPULayers {
			return mismatchf("%d of %s layers were offloaded to the GPU, but the plan has %d.", got, last[2], p.GPULayers)
		}
	} else {
		return mismatchf("the backend did not report how many layers it offloaded.")
	}

	// Experts: exactly the planned set in host memory.
	want := map[string]bool{}
	for _, n := range p.HostExperts {
		want[n] = true
	}
	host, device := map[string]bool{}, map[string]string{}
	for _, m := range overrideRe.FindAllSubmatch(log, -1) {
		name, buft := string(m[1]), string(m[2])
		if !expertRe.MatchString(name) {
			continue
		}
		if strings.HasPrefix(buft, "CPU") || buft == "CUDA_Host" {
			host[name] = true
			delete(device, name)
		} else {
			device[name] = buft
			delete(host, name)
		}
	}
	var onDevice, missing, extra []string
	for n := range want {
		switch {
		case device[n] != "":
			onDevice = append(onDevice, n+" on "+device[n])
		case !host[n]:
			missing = append(missing, n)
		}
	}
	for n := range host {
		if !want[n] {
			extra = append(extra, n)
		}
	}
	sort.Strings(onDevice)
	sort.Strings(missing)
	sort.Strings(extra)
	switch {
	case len(onDevice) > 0:
		return mismatchf("expert tensors planned for system memory were loaded on the GPU: %s.", list(onDevice))
	case len(missing) > 0:
		return mismatchf("expert tensors planned for system memory were not placed there: %s.", list(missing))
	case len(extra) > 0:
		return mismatchf("expert tensors outside the plan were placed in system memory: %s.", list(extra))
	}

	// Workspace: the GPU compute buffer within the planned allowance.
	if p.Platform == Discrete && p.GPULayers > 0 {
		if all := computeRe.FindAllSubmatch(log, -1); len(all) > 0 {
			mibs, _ := strconv.ParseFloat(string(all[len(all)-1][1]), 64)
			got := int64(mibs * (1 << 20))
			allowed := p.WorkspaceBytes + max(p.WorkspaceBytes/10, workspaceSlack)
			if got > allowed {
				return mismatchf("the GPU compute buffer is %.0f MiB, but the plan allows %d MiB (a %d MiB workspace plus a margin).",
					mibs, allowed>>20, p.WorkspaceBytes>>20)
			}
		}
	}
	return nil
}

func list(xs []string) string {
	if len(xs) > 6 {
		return strings.Join(xs[:6], ", ") + fmt.Sprintf(" and %d more", len(xs)-6)
	}
	return strings.Join(xs, ", ")
}

// LastLines is the tail of a log for an error report, with invalid bytes
// replaced.
func LastLines(log []byte, n int) string {
	lines := bytes.Split(bytes.TrimRight(log, "\n"), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.ToValidUTF8(string(bytes.Join(lines, []byte("\n"))), "�")
}
