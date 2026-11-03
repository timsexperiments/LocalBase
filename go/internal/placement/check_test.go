package placement

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// planN4 is the qualified GPT-OSS-20B plan: experts of layers 0-3 on host.
func planN4(t *testing.T) Plan {
	t.Helper()
	m, _ := loadGptoss(t)
	four := 4
	p, err := Resolve(m, 1721303040, Device{Platform: Discrete, FreeVRAM: 11899 * mib, HostAvailable: 64 << 30, CPUMoE: true, GPULayers: true},
		Request{Mode: Custom, CPUMoELayers: &four})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// syntheticLog writes llama-server's startup lines: overrides for the given
// tensors, the offload count and the compute buffers.
func syntheticLog(overrides map[string]string, offloaded, total int, cudaComputeMiB float64) []byte {
	var b strings.Builder
	b.WriteString("0.00.100.000 I build: b11671 (ba3c05f71) with cc for x86_64\n")
	for name, buft := range overrides {
		fmt.Fprintf(&b, "0.00.200.000 I tensor %s (2880 x 2880 x 32 mxfp4) buffer type overridden to %s\n", name, buft)
	}
	fmt.Fprintf(&b, "0.00.351.819 I load_tensors: offloaded %d/%d layers to GPU\n", offloaded, total)
	// llama.cpp reserves twice; the last report counts.
	b.WriteString("0.00.360.781 I sched_reserve:      CUDA0 compute buffer size =     0.00 MiB\n")
	fmt.Fprintf(&b, "0.00.891.423 I sched_reserve:      CUDA0 compute buffer size = %8.2f MiB\n", cudaComputeMiB)
	b.WriteString("0.00.891.423 I sched_reserve:  CUDA_Host compute buffer size =     6.52 MiB\n")
	return []byte(b.String())
}

func experts(p Plan, buft string) map[string]string {
	o := map[string]string{}
	for _, n := range p.HostExperts {
		o[n] = buft
	}
	return o
}

func mismatch(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Code == "placement_mismatch"
}

func TestCheckAcceptsHostExperts(t *testing.T) {
	p := planN4(t)
	if len(p.HostExperts) != 24 {
		t.Fatalf("plan has %d host experts", len(p.HostExperts))
	}
	for _, buft := range []string{"CPU", "CPU_Mapped", "CUDA_Host"} {
		if err := Check(syntheticLog(experts(p, buft), 25, 25, 200), p); err != nil {
			t.Errorf("%s: %v", buft, err)
		}
	}
}

func TestCheckRejectsDeviceExperts(t *testing.T) {
	p := planN4(t)
	o := experts(p, "CPU")
	o["blk.2.ffn_gate_exps.weight"] = "CUDA0"
	err := Check(syntheticLog(o, 25, 25, 200), p)
	if !mismatch(err) || !strings.Contains(err.Error(), "blk.2.ffn_gate_exps.weight") || !strings.Contains(err.Error(), "CUDA0") {
		t.Errorf("an expert on CUDA0 passed: %v", err)
	}
}

func TestCheckRejectsMissingAndExtraExperts(t *testing.T) {
	p := planN4(t)
	o := experts(p, "CPU")
	delete(o, "blk.3.ffn_up_exps.bias")
	if err := Check(syntheticLog(o, 25, 25, 200), p); !mismatch(err) || !strings.Contains(err.Error(), "blk.3.ffn_up_exps.bias") {
		t.Errorf("a missing override passed: %v", err)
	}
	o = experts(p, "CPU")
	o["blk.4.ffn_down_exps.weight"] = "CPU" // layer 4 was planned on the GPU
	if err := Check(syntheticLog(o, 25, 25, 200), p); !mismatch(err) || !strings.Contains(err.Error(), "blk.4.ffn_down_exps.weight") {
		t.Errorf("an extra host expert passed: %v", err)
	}
}

func TestCheckLayersAndWorkspace(t *testing.T) {
	m := denseModel(10, 100*mib)
	five := 5
	p, err := Resolve(m, 100*mib, Device{Platform: Discrete, FreeVRAM: 8 << 30, HostAvailable: 8 << 30, CPUMoE: true, GPULayers: true},
		Request{Mode: Custom, GPULayers: &five})
	if err != nil {
		t.Fatal(err)
	}
	if err := Check(syntheticLog(nil, 5, 11, 250), p); err != nil {
		t.Errorf("matching layers: %v", err)
	}
	if err := Check(syntheticLog(nil, 7, 11, 250), p); !mismatch(err) || !strings.Contains(err.Error(), "7") {
		t.Errorf("7/11 layers passed for a plan of 5: %v", err)
	}
	// The planned workspace is 256 MiB; up to 10% or 64 MiB over is allowed.
	if err := Check(syntheticLog(nil, 5, 11, 300), p); err != nil {
		t.Errorf("300 MiB compute buffer (within 64 MiB): %v", err)
	}
	err = Check(syntheticLog(nil, 5, 11, 512), p)
	if !mismatch(err) || !strings.Contains(err.Error(), "512") || !strings.Contains(err.Error(), "256") {
		t.Errorf("a 512 MiB compute buffer passed against 256 planned: %v", err)
	}
	if err := Check([]byte("no startup lines\n"), p); !mismatch(err) {
		t.Errorf("a log without the offload line passed: %v", err)
	}
}

// The log is bytes: invalid UTF-8 (a strict decode once crashed a harness)
// neither panics nor hides the lines around it.
func TestCheckInvalidUTF8(t *testing.T) {
	p := planN4(t)
	log := append([]byte("\xff\xfe garbage \xc3\x28\n"), syntheticLog(experts(p, "CPU"), 25, 25, 200)...)
	log = append(log, []byte("\x80\x81 trailing\n")...)
	if err := Check(log, p); err != nil {
		t.Errorf("valid lines among invalid bytes: %v", err)
	}
	bad := experts(p, "CPU")
	bad["blk.0.ffn_down_exps.weight"] = "CUDA0"
	if err := Check(append([]byte("\xff\n"), syntheticLog(bad, 25, 25, 200)...), p); !mismatch(err) {
		t.Errorf("invalid bytes hid a mismatch: %v", err)
	}
}

// A full-GPU plan checks nothing about experts and accepts its own log.
func TestCheckFullGPU(t *testing.T) {
	m := denseModel(10, 100*mib)
	p, _ := Resolve(m, 100*mib, Device{Platform: Discrete, FreeVRAM: 8 << 30, HostAvailable: 8 << 30, CPUMoE: true, GPULayers: true}, Request{Mode: Auto})
	if err := Check(syntheticLog(nil, 11, 11, 200), p); err != nil {
		t.Errorf("full GPU: %v", err)
	}
}

// Real llama-server CUDA startup lines (copied from the qualification
// evidence, a dense 32-layer model fully on the GPU): the parser reads
// llama.cpp's own format, with the dry-run reserve before the real one.
func TestCheckRealLogLines(t *testing.T) {
	log, err := os.ReadFile("testdata/cuda-startup-lines.log")
	if err != nil {
		t.Fatal(err)
	}
	p := Plan{Platform: Discrete, GPULayers: 33, TotalLayers: 33, WorkspaceBytes: WorkspaceBytes}
	if err := Check(log, p); err != nil {
		t.Errorf("real full-GPU log: %v", err)
	}
	p.GPULayers = 20
	if err := Check(log, p); !mismatch(err) {
		t.Errorf("33/33 offloaded passed a plan of 20: %v", err)
	}
	p.GPULayers, p.WorkspaceBytes = 33, 0 // the real 36.52 MiB buffer exceeds a 0 MiB plan by less than 64 MiB
	if err := Check(log, p); err != nil {
		t.Errorf("within the slack: %v", err)
	}
}

func TestRefusalOnAMachineWithNoGPUGivesNoGPUAdvice(t *testing.T) {
	m := denseModel(10, 100*mib)
	_, err := Resolve(m, 100*mib, Device{Platform: CPUOnly, HostAvailable: 1 << 20, KeptBytes: 8 << 30, CapacityBytes: 64 << 30}, Request{Mode: Auto})
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != "placement_does_not_fit" {
		t.Fatalf("%v", err)
	}
	if strings.Contains(pe.Message, "GPU") {
		t.Errorf("GPU advice with no GPU: %s", pe.Message)
	}
	if !strings.Contains(pe.Message, "Close other applications") {
		t.Errorf("a shortage that can clear should say how: %s", pe.Message)
	}
	_, err = Resolve(m, 100*mib, Device{Platform: Discrete, GPUCount: 1, GPULayers: true, HostAvailable: 1 << 20, FreeVRAM: 8 << 30, CapacityBytes: 64 << 30}, Request{Mode: CPU})
	if !errors.As(err, &pe) || !strings.Contains(pe.Message, "GPU") {
		t.Errorf("a machine with a GPU keeps that advice: %v", err)
	}
}

// Even idle it cannot fit: a different code, and the numbers and what to do.
func TestRefusalThatNoWaitCanClearIsTooLarge(t *testing.T) {
	m := denseModel(10, 100*mib)
	m.HostExtraBytes = 7 << 30
	_, err := Resolve(m, 100*mib, Device{Platform: CPUOnly, HostAvailable: 7 << 30, KeptBytes: 8 << 30, CapacityBytes: 15 << 30}, Request{Mode: Auto})
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != "model_too_large" {
		t.Fatalf("%v", err)
	}
	for _, want := range []string{"15.0 GB", "8.0 GB", "7.0 GB", "smaller model", "context", "memory.systemReserve"} {
		if !strings.Contains(pe.Message, want) {
			t.Errorf("no %q in: %s", want, pe.Message)
		}
	}
	if strings.Contains(pe.Message, "GPU") {
		t.Errorf("GPU advice with no GPU: %s", pe.Message)
	}
}
