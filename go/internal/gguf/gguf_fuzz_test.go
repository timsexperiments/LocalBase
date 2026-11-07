package gguf

import (
	"bytes"
	"testing"
)

// FuzzGGUFHeader feeds arbitrary bytes to the header and tensor table reader. Plain
// `go test` runs the seeds; tools/fuzz.sh fuzzes on demand. It must never panic,
// overflow the stack or keep more than its budget.
func FuzzGGUFHeader(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("GGUF"))
	f.Add(newGGUF(0, 0).b.Bytes())
	f.Add(nestedArrays(3))
	f.Add(newGGUF(1<<40, 1<<40).b.Bytes())
	f.Add(newGGUF(0, 1).str("general.architecture").put(uint32(ggufTypeString)).str("qwen3").b.Bytes())
	f.Add(newGGUF(0, 4).str("general.architecture").put(uint32(ggufTypeString)).str("gemma4").
		str("gemma4.block_count").put(uint32(4), uint32(2)).
		str("gemma4.attention.head_count").put(uint32(ggufTypeArray), uint32(4), uint64(2), uint32(8), uint32(8)).
		str("gemma4.attention.sliding_window").put(uint32(4), uint32(4096)).b.Bytes())
	f.Add(newGGUF(1, 0).str("t").put(uint32(1), uint64(4), uint32(0), uint64(0)).b.Bytes())
	f.Add(newGGUF(0, 1).str("tokens").put(uint32(ggufTypeArray), uint32(ggufTypeString), uint64(1<<40)).b.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		values, tensors, err := parseAll(bytes.NewReader(data))
		if len(tensors) > maxTensorCount {
			t.Fatalf("%d tensors", len(tensors))
		}
		kept := 0
		for _, v := range values {
			if s, ok := v.(string); ok {
				kept += len(s)
			}
		}
		if kept > maxKeptBytes {
			t.Fatalf("kept %d bytes", kept)
		}
		if err != nil {
			return
		}
		if g := GeometryFromMetadata(values); g != nil {
			KVCacheBytes(g, 65536, 4, "f16", "q8_0")
		}
	})
}
