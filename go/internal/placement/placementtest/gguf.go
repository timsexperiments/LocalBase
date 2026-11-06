// Package placementtest writes synthetic GGUF headers for placement tests.
package placementtest

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// WriteMoEGGUF replaces path with a GGUF header for a 2-layer MoE model
// (1 GiB of F32 experts per layer), sparse-extended to size. No weights.
func WriteMoEGGUF(t testing.TB, path string, size int64) {
	t.Helper()
	var b bytes.Buffer
	w := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { w(uint64(len(s))); b.WriteString(s) }
	u32 := func(k string, v uint32) { str(k); w(uint32(4)); w(v) }
	type tensor struct {
		name string
		dims []uint64
	}
	tensors := []tensor{
		{"blk.0.ffn_down_exps.weight", []uint64{8192, 8192, 4}},
		{"blk.1.ffn_down_exps.weight", []uint64{8192, 8192, 4}},
		{"blk.0.attn_q.weight", []uint64{4096, 1024}},
		{"blk.1.attn_q.weight", []uint64{4096, 1024}},
		{"output.weight", []uint64{4096, 1024}},
	}
	w(uint32(0x46554747))
	w(uint32(3))
	w(uint64(len(tensors)))
	w(uint64(8))
	str("general.architecture")
	w(uint32(8))
	str("gpt-oss")
	u32("gpt-oss.block_count", 2)
	u32("gpt-oss.context_length", 131072)
	u32("gpt-oss.embedding_length", 2880)
	u32("gpt-oss.attention.head_count", 64)
	u32("gpt-oss.attention.head_count_kv", 8)
	u32("gpt-oss.attention.key_length", 64)
	u32("gpt-oss.attention.value_length", 64)
	for _, x := range tensors {
		str(x.name)
		w(uint32(len(x.dims)))
		for _, d := range x.dims {
			w(d)
		}
		w(uint32(0))
		w(uint64(0))
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}
