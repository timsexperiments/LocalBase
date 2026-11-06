package runtime

import (
	"slices"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/placement"
)

func TestAutomaticPlacementOnAMacAddsNoLogLevel(t *testing.T) {
	p := &LLMPlan{ModelPath: "m", Host: "127.0.0.1", Port: 1, CtxSize: 2048, KVType: "q8_0", PromptCacheMiB: 1}
	p.Parallel.Slots = 1
	p.Placement = &placement.Plan{Mode: placement.Auto, Platform: placement.Unified, TotalLayers: 29, GPULayers: 29}
	if a := p.Args(true); slices.Contains(a, "-lv") || !slices.Contains(a, "--embeddings") {
		t.Errorf("the live launch changed: %v", a)
	}
}

// --embeddings with -ngl 0 crashes the first chat on a Mac (llama-server 0.7.0-dev,
// build 11671, Metal: EXC_BAD_ACCESS in ggml_gallocr_alloc_graph). Any layer on the GPU
// is fine, and so is the same launch on Linux.
func TestMacCPUPlacementOmitsEmbeddings(t *testing.T) {
	p := &LLMPlan{ModelPath: "m", Host: "127.0.0.1", Port: 1, CtxSize: 2048, KVType: "q8_0", PromptCacheMiB: 1}
	p.Parallel.Slots = 1
	p.Placement = &placement.Plan{Mode: placement.CPU, Platform: placement.Unified, GPULayers: 0, Args: []string{"-ngl", "0"}}
	if slices.Contains(p.Args(true), "--embeddings") {
		t.Error("a Mac CPU launch has --embeddings")
	}
	if !slices.Contains(p.Args(false), "--embeddings") {
		t.Error("a Linux CPU launch lost --embeddings")
	}
	p.Placement.GPULayers = 1
	if !slices.Contains(p.Args(true), "--embeddings") {
		t.Error("a Mac launch with a GPU layer lost --embeddings")
	}
}
