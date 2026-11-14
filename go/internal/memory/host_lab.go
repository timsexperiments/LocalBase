//go:build localbase_lab

package memory

import (
	"context"
	"encoding/json"
	"os"
)

// The reliability lab's host (go/test/lab): a localbase_lab build only, never
// a release. When LOCALBASE_LAB_HOST_FILE names a file, the gateway reads its
// memory from that file, so a harness can change available memory and pressure
// under a real process without using any memory. Topology is read once, as on
// a real machine; availability on every sample. A file that cannot be read is
// an unavailable measurement.

const labHostEnv = "LOCALBASE_LAB_HOST_FILE"

type labHostFile struct {
	CapacityBytes      int64  `json:"capacityBytes"`
	Unified            bool   `json:"unified"`
	GPUWorkingSetBytes int64  `json:"gpuWorkingSetBytes"`
	AvailableBytes     int64  `json:"availableBytes"`
	Pressure           string `json:"pressure"`
}

type labHostProvider struct {
	path string
	topo Topology
}

func readLabHost(path string) (labHostFile, error) {
	var f labHostFile
	b, err := os.ReadFile(path)
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal(b, &f)
}

func (p *labHostProvider) Topology() Topology { return p.topo }

func (p *labHostProvider) Snapshot(context.Context) Snapshot {
	f, err := readLabHost(p.path)
	if err != nil {
		return Snapshot{Available: -1, Pressure: "unknown"}
	}
	return Snapshot{Available: f.AvailableBytes, Pressure: f.Pressure}
}

func init() {
	path := os.Getenv(labHostEnv)
	if path == "" {
		return
	}
	labHost = func() HostProvider {
		f, err := readLabHost(path)
		if err != nil {
			panic("lab host: " + err.Error())
		}
		return &labHostProvider{path: path, topo: Topology{Unified: f.Unified, SystemCapacity: f.CapacityBytes, GPUWorkingSetBytes: f.GPUWorkingSetBytes}}
	}
}
