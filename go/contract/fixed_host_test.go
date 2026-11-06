package contract

import (
	"encoding/json"
	"path/filepath"
	goruntime "runtime"
	"testing"
)

const gib = int64(1) << 30

// onFixedHost runs e's gateway and commands as a localbase_lab build that reads its memory from a
// file instead of the machine (internal/memory/host_lab.go): capacityGiB of memory, availableMiB of
// it free, unified on Apple silicon as the machine is. A test whose model must fit uses it, so what
// it checks doesn't turn on how much a CI runner happens to have free at that moment.
func onFixedHost(t *testing.T, e *e2e, capacityGiB, availableMiB int64) {
	t.Helper()
	unified := goruntime.GOOS == "darwin" && goruntime.GOARCH == "arm64"
	host := map[string]any{"capacityBytes": capacityGiB * gib, "unified": unified, "availableBytes": availableMiB << 20, "pressure": "normal"}
	if unified {
		host["gpuWorkingSetBytes"] = capacityGiB * gib * 3 / 4
	}
	b, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(e.outDir, "lab-host.json")
	if err := mustWriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	e.lab = true
	e.env = append(e.env, "LOCALBASE_LAB_HOST_FILE="+path)
}

// The fixed host is what the gateway sees: if the lab build or the file were left out, it would
// report this machine's memory instead.
func TestAFixedHostIsWhatTheGatewaySees(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	tmp := scratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true}
	onFixedHost(t, e, 16, 12345)
	if r := e.cli(nil, "--non-interactive", "init"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	e.serve("--no-auth")
	defer e.stop()
	r := e.do("GET", "/_localbase/host", "")
	if r.Status != 200 {
		t.Fatalf("host: %d %s", r.Status, r.Body)
	}
	var body struct {
		Host struct {
			Memory struct {
				Total     struct{ Bytes *int64 } `json:"total"`
				Available struct{ Bytes *int64 } `json:"available"`
			} `json:"memory"`
		} `json:"host"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	m := body.Host.Memory
	if m.Total.Bytes == nil || *m.Total.Bytes != 16*gib || m.Available.Bytes == nil || *m.Available.Bytes != 12345<<20 {
		t.Errorf("the gateway's memory isn't the fixed host's: %s", r.Body)
	}
}
