package placement

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

const mib = 1 << 20

// The GPT-OSS-20B fixtures are the header-derived evidence of the operator's
// Pixelator qualification (internal; DESIGN §20.3): the tensor table and the
// architecture metadata, no weights.
func loadGptoss(t *testing.T) (Model, *memory.Geometry) {
	t.Helper()
	var tf struct {
		Tensors []struct {
			Name       string   `json:"name"`
			Dimensions []uint64 `json:"dimensions"`
			Type       uint32   `json:"type"`
		} `json:"tensors"`
	}
	b, err := os.ReadFile("testdata/gptoss20b-tensors.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &tf); err != nil {
		t.Fatal(err)
	}
	m := Model{ArtifactBytes: 11624759488, Layers: 24}
	for _, x := range tf.Tensors {
		m.Tensors = append(m.Tensors, Tensor{Name: x.Name, Dims: x.Dimensions, Type: x.Type})
	}
	var mf struct {
		Metadata map[string]any `json:"metadata"`
	}
	b, _ = os.ReadFile("testdata/gptoss20b-metadata.json")
	json.Unmarshal(b, &mf)
	return m, memory.GeometryFromMetadata(mf.Metadata)
}

// The planner reproduces the qualified GPT-OSS-20B offload from the header
// alone: 144 expert tensors over 24 layers, 424,120,320 B each, and the
// minimum --n-cpu-moe per context against 11,899 MiB of free VRAM.
func TestGptossGolden(t *testing.T) {
	m, g := loadGptoss(t)
	ex := Experts(m.Tensors)
	if ex.Tensors != 144 || len(ex.ByLayer) != 24 {
		t.Fatalf("experts: %d tensors over %d layers", ex.Tensors, len(ex.ByLayer))
	}
	for l, b := range ex.ByLayer {
		if b != 424120320 {
			t.Errorf("layer %d expert bytes %d", l, b)
		}
	}
	dev := Device{Platform: Discrete, FreeVRAM: 11899 * mib, HostAvailable: 64 << 30, CPUMoE: true, GPULayers: true}
	for _, c := range []struct {
		ctx  int
		kv   int64
		minN int
	}{
		{5120, 76873728, 1}, {9216, 130351104, 1}, {17408, 237305856, 1},
		{33792, 451215360, 1}, {66560, 879034368, 2}, {131072, 1721303040, 4},
	} {
		kv := memory.KVCacheBytes(g, c.ctx, 1, "q8_0", "q8_0")
		if kv != c.kv {
			t.Errorf("ctx %d: KV %d, want %d", c.ctx, kv, c.kv)
		}
		p, err := Resolve(m, kv, dev, Request{Mode: Auto})
		if err != nil {
			t.Fatalf("ctx %d: %v", c.ctx, err)
		}
		if p.CPUMoELayers != c.minN || p.GPULayers != p.TotalLayers {
			t.Errorf("ctx %d: auto chose %d expert layers on host, %d/%d GPU layers; want %d", c.ctx, p.CPUMoELayers, p.GPULayers, p.TotalLayers, c.minN)
		}
		if p.GPUBytes+p.ReserveBytes > dev.FreeVRAM {
			t.Errorf("ctx %d: plan needs %d of %d", c.ctx, p.GPUBytes+p.ReserveBytes, dev.FreeVRAM)
		}
	}
	// The qualified fixed N = 4 for the whole ladder.
	four := 4
	p, err := Resolve(m, 1721303040, dev, Request{Mode: Custom, CPUMoELayers: &four})
	if err != nil {
		t.Fatal(err)
	}
	if p.HostBytes != 1696481280 || p.HostTensors != 24 || !slices.Equal(p.HostLayers, []int{0, 1, 2, 3}) {
		t.Errorf("N=4: host %d B, %d tensors, layers %v", p.HostBytes, p.HostTensors, p.HostLayers)
	}
	if !slices.Equal(p.Args, []string{"--n-cpu-moe", "4"}) {
		t.Errorf("args %v", p.Args)
	}
	// Full GPU does not fit at 4K: 11086 + 73 + 256 + 512 MiB > 11899 MiB.
	if _, err := Resolve(m, 76873728, dev, Request{Mode: GPU}); !isCode(err, "placement_does_not_fit") {
		t.Errorf("gpu-only at 4K: %v", err)
	}
}

func isCode(err error, code string) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Code == code
}

// Tensor bytes follow ggml's block sizes.
func TestTensorBytes(t *testing.T) {
	for _, c := range []struct {
		t    Tensor
		want int64
	}{
		{Tensor{Dims: []uint64{2880, 2880, 32}, Type: 39}, 141004800}, // MXFP4: 17 B per 32 values
		{Tensor{Dims: []uint64{2880, 32}, Type: 0}, 368640},           // F32
		{Tensor{Dims: []uint64{4096, 2880}, Type: 12}, 6635520},       // Q4_K: 144 B per 256
		{Tensor{Dims: []uint64{512}, Type: 8}, 544},                   // Q8_0: 34 B per 32
	} {
		if got, ok := TensorBytes(c.t); !ok || got != c.want {
			t.Errorf("%v: %d %v, want %d", c.t, got, ok, c.want)
		}
	}
	if _, ok := TensorBytes(Tensor{Dims: []uint64{4}, Type: 999}); ok {
		t.Error("an unknown type has a size")
	}
}

// A dense model: auto offloads repeating layers (-ngl) when the whole model
// does not fit; GPU-only never falls back; CPU-only is -ngl 0.
func TestDensePlacement(t *testing.T) {
	m := denseModel(10, 100*mib)                                                                                   // 10 layers of 100 MiB, output 50 MiB, embeddings 50 MiB
	dev := Device{Platform: Discrete, FreeVRAM: 2000 * mib, HostAvailable: 8 << 30, CPUMoE: true, GPULayers: true} // 1100 + 100 KV + 256 + 512 MiB fits
	p, err := Resolve(m, 100*mib, dev, Request{Mode: Auto})
	if err != nil || p.GPULayers != p.TotalLayers || len(p.Args) != 0 {
		t.Fatalf("fits: %+v %v", p, err)
	}
	dev.FreeVRAM = 1500 * mib
	p, err = Resolve(m, 100*mib, dev, Request{Mode: Auto})
	if err != nil || p.GPULayers >= p.TotalLayers || p.GPULayers == 0 || p.CPUMoELayers != 0 {
		t.Fatalf("partial: %+v %v", p, err)
	}
	if p.GPUBytes+p.ReserveBytes > dev.FreeVRAM || p.HostBytes == 0 {
		t.Errorf("partial plan %+v", p)
	}
	if _, err := Resolve(m, 100*mib, dev, Request{Mode: GPU}); !isCode(err, "placement_does_not_fit") {
		t.Errorf("gpu-only fell back: %v", err)
	}
	p, err = Resolve(m, 100*mib, dev, Request{Mode: CPU})
	if err != nil || p.GPULayers != 0 || !slices.Equal(p.Args, []string{"-ngl", "0"}) {
		t.Errorf("cpu-only: %+v %v", p, err)
	}
	one := 1
	if _, err := Resolve(m, 100*mib, dev, Request{Mode: Custom, CPUMoELayers: &one}); !isCode(err, "placement_unsupported") {
		t.Errorf("n-cpu-moe on a dense model: %v", err)
	}
	dev.FreeVRAM = 300 * mib
	if _, err := Resolve(m, 100*mib, dev, Request{Mode: Auto}); err != nil {
		t.Errorf("auto with little VRAM: %v", err) // still places some or zero layers
	}
}

// Unified memory: no VRAM planning; experts on the CPU are refused; -ngl
// and CPU-only are plain launch choices.
func TestUnifiedPlacement(t *testing.T) {
	m, _ := loadGptoss(t)
	dev := Device{Platform: Unified, CPUMoE: true, GPULayers: true}
	p, err := Resolve(m, 76873728, dev, Request{Mode: Auto})
	if err != nil || len(p.Args) != 0 || p.Note == "" || p.GPULayers != p.TotalLayers {
		t.Fatalf("auto: %+v %v", p, err)
	}
	two := 2
	if _, err := Resolve(m, 76873728, dev, Request{Mode: Custom, CPUMoELayers: &two}); !isCode(err, "placement_unsupported") {
		t.Errorf("n-cpu-moe on unified memory: %v", err)
	}
	ten := 10
	p, err = Resolve(m, 76873728, dev, Request{Mode: Custom, GPULayers: &ten})
	if err != nil || !slices.Equal(p.Args, []string{"-ngl", "10"}) {
		t.Errorf("custom gpu layers: %+v %v", p, err)
	}
}

// Unsupported overrides fail with actionable messages.
func TestUnsupportedOverrides(t *testing.T) {
	m, _ := loadGptoss(t)
	four, big, neg := 4, 99, -1
	cases := []struct {
		dev Device
		req Request
	}{
		{Device{Platform: Discrete, FreeVRAM: 12 << 30, HostAvailable: 64 << 30, CPUMoE: false, GPULayers: true}, Request{Mode: Custom, CPUMoELayers: &four}},
		{Device{Platform: CPUOnly, HostAvailable: 64 << 30, CPUMoE: true}, Request{Mode: Custom, GPULayers: &four}},
		{Device{Platform: CPUOnly, HostAvailable: 64 << 30, CPUMoE: true}, Request{Mode: GPU}},
		{Device{Platform: Discrete, FreeVRAM: 12 << 30, HostAvailable: 64 << 30, CPUMoE: true, GPULayers: true}, Request{Mode: Custom, CPUMoELayers: &big}},
		{Device{Platform: Discrete, FreeVRAM: 12 << 30, HostAvailable: 64 << 30, CPUMoE: true, GPULayers: true}, Request{Mode: Custom, GPULayers: &neg}},
		{Device{Platform: Discrete, FreeVRAM: 12 << 30, HostAvailable: 64 << 30, CPUMoE: true, GPULayers: true, GPUCount: 2}, Request{Mode: Auto}},
		{Device{Platform: Discrete, FreeVRAM: 12 << 30, HostAvailable: 64 << 30}, Request{Mode: "fastest"}},
	}
	for i, c := range cases {
		_, err := Resolve(m, 76873728, c.dev, c.req)
		var pe *Error
		if !errors.As(err, &pe) || pe.Code != "placement_unsupported" || pe.Message == "" {
			t.Errorf("case %d: %v", i, err)
		}
	}
	// Host memory too small for the offload: does not fit, with the numbers.
	dev := Device{Platform: Discrete, FreeVRAM: 11899 * mib, HostAvailable: 1 << 30, CPUMoE: true, GPULayers: true}
	_, err := Resolve(m, 1721303040, dev, Request{Mode: Auto})
	if !isCode(err, "placement_does_not_fit") {
		t.Errorf("host too small: %v", err)
	}
}

func denseModel(layers int, perLayer int64) Model {
	m := Model{Layers: layers}
	// One F32 tensor per layer of perLayer bytes.
	for i := 0; i < layers; i++ {
		m.Tensors = append(m.Tensors, Tensor{Name: blk(i, "attn_q.weight"), Dims: []uint64{uint64(perLayer / 4)}, Type: 0})
	}
	m.Tensors = append(m.Tensors, Tensor{Name: "output.weight", Dims: []uint64{uint64(50 * mib / 4)}, Type: 0},
		Tensor{Name: "token_embd.weight", Dims: []uint64{uint64(50 * mib / 4)}, Type: 0})
	for _, x := range m.Tensors {
		b, _ := TensorBytes(x)
		m.ArtifactBytes += b
	}
	return m
}

func blk(i int, s string) string { return "blk." + strconv.Itoa(i) + "." + s }
