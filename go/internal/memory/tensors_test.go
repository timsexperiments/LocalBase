package memory

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// ReadTensorTable reads names, shapes and types after the key block.
func TestReadTensorTable(t *testing.T) {
	var b bytes.Buffer
	w := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { w(uint64(len(s))); b.WriteString(s) }
	w(uint32(ggufMagic))
	w(uint32(3))
	w(uint64(2)) // tensors
	w(uint64(1)) // keys
	str("general.architecture")
	w(uint32(8)) // string
	str("gpt-oss")
	for _, x := range []struct {
		name string
		dims []uint64
		typ  uint32
	}{{"blk.0.ffn_down_exps.weight", []uint64{2880, 2880, 32}, 39}, {"output_norm.weight", []uint64{2880}, 0}} {
		str(x.name)
		w(uint32(len(x.dims)))
		for _, d := range x.dims {
			w(d)
		}
		w(x.typ)
		w(uint64(0))
	}
	path := filepath.Join(t.TempDir(), "m.gguf")
	os.WriteFile(path, b.Bytes(), 0o600)
	got, err := ReadTensorTable(path)
	if err != nil || len(got) != 2 || got[0].Name != "blk.0.ffn_down_exps.weight" || got[0].Type != 39 || len(got[0].Dims) != 3 || got[1].Dims[0] != 2880 {
		t.Fatalf("%+v %v", got, err)
	}
	if keys, err := ReadGGUFHeader(path, func(string) bool { return true }); err != nil || keys["general.architecture"] != "gpt-oss" {
		t.Errorf("keys %v %v", keys, err)
	}
}
