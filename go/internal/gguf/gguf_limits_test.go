package gguf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// ggufBuilder writes GGUF headers, including ones no honest tool writes.
type ggufBuilder struct{ b bytes.Buffer }

func newGGUF(tensors, keys uint64) *ggufBuilder {
	g := &ggufBuilder{}
	g.put(uint32(ggufMagic), uint32(3), tensors, keys)
	return g
}

func (g *ggufBuilder) put(vs ...any) *ggufBuilder {
	for _, v := range vs {
		binary.Write(&g.b, binary.LittleEndian, v)
	}
	return g
}

func (g *ggufBuilder) str(s string) *ggufBuilder {
	g.put(uint64(len(s)))
	g.b.WriteString(s)
	return g
}

func parseAll(src io.Reader) (map[string]any, []TensorInfo, error) {
	return parseHeader(src, func(string) bool { return true }, func(string) int { return maxChatTemplateBytes }, true)
}

// nestedArrays is an array of arrays, depth levels deep.
func nestedArrays(depth int) []byte {
	g := newGGUF(0, 1).str("k").put(uint32(ggufTypeArray))
	for range depth {
		g.put(uint32(ggufTypeArray), uint64(1))
	}
	return g.b.Bytes()
}

func TestNestedArraysAreRefused(t *testing.T) {
	for _, depth := range []int{1, 2, 1000} {
		if _, _, err := parseAll(bytes.NewReader(nestedArrays(depth))); !errors.Is(err, ErrNestedGGUFArray) {
			t.Errorf("depth %d: %v, want ErrNestedGGUFArray", depth, err)
		}
	}
}

// The reader's stack does not grow with its input. If arrays recursed again, this
// would end the test binary with a stack overflow, at any depth well under the
// default limit: that is the failure this guards against.
func TestDeepNestingUsesNoStack(t *testing.T) {
	defer debug.SetMaxStack(debug.SetMaxStack(1 << 20))
	done := make(chan error)
	go func() { // a fresh goroutine starts with a small stack
		_, _, err := parseAll(bytes.NewReader(nestedArrays(500_000)))
		done <- err
	}()
	if err := <-done; !errors.Is(err, ErrNestedGGUFArray) {
		t.Fatalf("%v, want ErrNestedGGUFArray", err)
	}
}

// Honest arrays still read: scalars, and strings to skip.
func TestArraysStillRead(t *testing.T) {
	g := newGGUF(0, 3).
		str("a.block_count").put(uint32(ggufTypeArray), uint32(4), uint64(2), uint32(7), uint32(9)).
		str("tokens").put(uint32(ggufTypeArray), uint32(ggufTypeString), uint64(2)).str("x").str("yz").
		str("empty").put(uint32(ggufTypeArray), uint32(4), uint64(0))
	values, _, err := parseAll(&g.b)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := values["a.block_count"].([]float64)
	if len(got) != 2 || got[0] != 7 || got[1] != 9 {
		t.Errorf("a.block_count: %v", values["a.block_count"])
	}
}

// Kept strings have a total budget, so memory does not follow the file's size.
func TestKeptStringsHaveABudget(t *testing.T) {
	value := strings.Repeat("x", maxChatTemplateBytes)
	var parts []io.Reader
	n := maxKeptBytes/maxChatTemplateBytes + 8
	parts = append(parts, &newGGUF(0, uint64(n)).b)
	for i := range n {
		key := newGGUF(0, 0)
		key.b.Reset()
		key.str(fmt.Sprintf("%s.%d", chatTemplateKey, i)).put(uint32(ggufTypeString), uint64(len(value)))
		parts = append(parts, &key.b, strings.NewReader(value))
	}
	values, _, err := parseAll(io.MultiReader(parts...))
	if err == nil {
		t.Fatalf("read %d keys without reaching the budget", len(values))
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// A header cannot ask for most of a huge file to be read just to step over it.
func TestSkippedBytesHaveABudget(t *testing.T) {
	huge := uint64(1 << 30)
	g := newGGUF(0, 0)
	g.b.Reset()
	g.put(uint32(ggufMagic), uint32(3), uint64(0), uint64(8))
	header := g.b.Bytes()
	entry := func(key string) []byte {
		e := &ggufBuilder{}
		return e.str(key).put(uint32(ggufTypeString), huge).b.Bytes()
	}
	var parts []io.Reader
	parts = append(parts, bytes.NewReader(header))
	for i := range 8 {
		parts = append(parts, bytes.NewReader(entry(fmt.Sprint("k", i))), io.LimitReader(zeros{}, int64(huge)))
	}
	_, _, err := parseHeader(io.MultiReader(parts...), func(string) bool { return false }, func(string) int { return 0 }, false)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("%v, want a refusal", err)
	}
}

// The tensor table grows with entries that arrive, not with the number claimed.
func TestTensorTableDoesNotTrustItsCount(t *testing.T) {
	data := newGGUF(maxTensorCount, 0).b.Bytes()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, err := parseAll(bytes.NewReader(data))
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("a table with no entries was read")
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
		t.Errorf("allocated %d MiB for a %d byte file", grew>>20, len(data))
	}
}

func TestTensorTableHasABudget(t *testing.T) {
	entry := &ggufBuilder{}
	entry.str("").put(uint32(0), uint32(0), uint64(0))
	data := append(newGGUF(maxTensorCount, 0).b.Bytes(), bytes.Repeat(entry.b.Bytes(), maxTensorCount)...)
	if _, tensors, err := parseAll(bytes.NewReader(data)); err == nil {
		t.Fatalf("kept %d tensor entries without reaching the budget", len(tensors))
	}
}

// Real tables still read.
func TestTensorTableStillReads(t *testing.T) {
	g := newGGUF(2, 0).
		str("blk.0.weight").put(uint32(2), uint64(4), uint64(8), uint32(0), uint64(0)).
		str("blk.1.weight").put(uint32(1), uint64(16), uint32(1), uint64(128))
	_, tensors, err := parseAll(&g.b)
	if err != nil || len(tensors) != 2 || tensors[1].Name != "blk.1.weight" || tensors[0].Dims[1] != 8 {
		t.Fatalf("%v %+v", err, tensors)
	}
}
