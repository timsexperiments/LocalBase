// Package testutil builds small model files for tests.
package testutil

import (
	"bytes"
	"encoding/binary"
	"os"
	"sort"
)

// GGUF writes a GGUF v3 file with the given header keys and no tensors,
// padded with zeros to at least size bytes. Values may be string, bool, int
// (written as uint32) or uint64.
func GGUF(path string, kv map[string]any, size int) error {
	var b bytes.Buffer
	w := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { w(uint64(len(s))); b.WriteString(s) }
	w(uint32(0x46554747))
	w(uint32(3))
	w(uint64(0))
	w(uint64(len(kv)))
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		str(k)
		switch v := kv[k].(type) {
		case string:
			w(uint32(8))
			str(v)
		case bool:
			w(uint32(7))
			if v {
				b.WriteByte(1)
			} else {
				b.WriteByte(0)
			}
		case int:
			w(uint32(4))
			w(uint32(v))
		case uint64:
			w(uint32(10))
			w(v)
		default:
			panic("unsupported GGUF test value")
		}
	}
	for b.Len() < size {
		b.WriteByte(0)
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}
