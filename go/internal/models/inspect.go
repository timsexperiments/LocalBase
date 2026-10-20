package models

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/memory"
)

// Inspection reads headers and small JSON sidecars only. It never imports,
// evaluates or runs anything from a model repository.

// FileFacts is what one file's own bytes say.
type FileFacts struct {
	Format        Fact[string]
	Architecture  Fact[string]
	Precision     Fact[string]
	ContextLength Fact[int]
	Dimensions    Fact[int]
	Role          string // projector, ... when the header says so
	SplitIndex    int    // 1-based shard number, 0 when not split
	SplitCount    int
	Problem       string // why nothing could be read
}

// GGUF general.file_type values (llama.cpp LLAMA_FTYPE).
var ggufFileTypes = map[int]string{
	0: "F32", 1: "F16", 2: "Q4_0", 3: "Q4_1", 7: "Q8_0", 8: "Q5_0", 9: "Q5_1", 10: "Q2_K",
	11: "Q3_K_S", 12: "Q3_K_M", 13: "Q3_K_L", 14: "Q4_K_S", 15: "Q4_K_M", 16: "Q5_K_S", 17: "Q5_K_M",
	18: "Q6_K", 19: "IQ2_XXS", 20: "IQ2_XS", 21: "Q2_K_S", 22: "IQ3_XS", 23: "IQ3_XXS", 24: "IQ1_S",
	25: "IQ4_NL", 26: "IQ3_S", 27: "IQ3_M", 28: "IQ2_S", 29: "IQ2_M", 30: "IQ4_XS", 31: "IQ1_M",
	32: "BF16", 36: "TQ1_0", 37: "TQ2_0", 38: "MXFP4_MOE",
}

var shardNameRe = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

const (
	ggufMagic        = 0x46554747 // "GGUF"
	ggmlMagic        = 0x67676d6c // whisper.cpp "ggml"
	maxSafetensorsHd = 100 << 20
)

func readMagic(path string) (uint32, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	var b [4]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return 0, info.Size(), errors.New("file is shorter than any model header")
	}
	return binary.LittleEndian.Uint32(b[:]), info.Size(), nil
}

// InspectFile identifies a file by its header. The extension is a hint for
// formats without a magic number (ONNX), never proof.
func InspectFile(path string) FileFacts {
	magic, size, err := readMagic(path)
	if err != nil {
		return FileFacts{Problem: err.Error()}
	}
	switch {
	case magic == ggufMagic:
		return inspectGGUF(path)
	case magic == ggmlMagic:
		return FileFacts{Format: Of("ggml", SourceInspect), Architecture: Of("whisper", SourceInspect)}
	}
	if ff, ok := inspectSafetensors(path, size); ok {
		return ff
	}
	if strings.EqualFold(filepath.Ext(path), ".onnx") {
		// ONNX is protobuf without a magic number: field 1 (ir_version,
		// varint) normally comes first.
		if f, err := os.Open(path); err == nil {
			var b [1]byte
			_, _ = f.Read(b[:])
			f.Close()
			if b[0] == 0x08 {
				return FileFacts{Format: Of("onnx", SourceInspect)}
			}
		}
	}
	return FileFacts{Problem: "unrecognized file format"}
}

func inspectGGUF(path string) FileFacts {
	values, err := memory.ReadGGUFHeader(path, func(k string) bool {
		return k == "general.architecture" || k == "general.file_type" || k == "split.count" || k == "split.no" ||
			k == "clip.projector_type" || k == "clip.has_vision_encoder" || k == "clip.has_audio_encoder" ||
			strings.HasSuffix(k, ".context_length") || strings.HasSuffix(k, ".embedding_length")
	})
	ff := FileFacts{Format: Of("gguf", SourceInspect)}
	if m := shardNameRe.FindStringSubmatch(filepath.Base(path)); m != nil {
		ff.SplitIndex, _ = strconv.Atoi(m[2])
		ff.SplitCount, _ = strconv.Atoi(m[3])
	}
	if err != nil {
		ff.Problem = "GGUF header could not be read: " + err.Error()
		return ff
	}
	num := func(k string) (int, bool) {
		f, ok := values[k].(float64)
		if !ok || f != math.Trunc(f) || f < 0 || f > 1e12 {
			return 0, false
		}
		return int(f), true
	}
	arch, _ := values["general.architecture"].(string)
	if arch != "" {
		ff.Architecture = Of(arch, SourceInspect)
	}
	if arch == "clip" || values["clip.has_vision_encoder"] == 1.0 || values["clip.has_audio_encoder"] == 1.0 {
		ff.Role = RoleProjector
	}
	if t, ok := num("general.file_type"); ok {
		if name, ok := ggufFileTypes[t]; ok {
			ff.Precision = Of(name, SourceInspect)
		}
	}
	if arch != "" {
		if n, ok := num(arch + ".context_length"); ok && n > 0 {
			ff.ContextLength = Of(n, SourceInspect)
		}
		if n, ok := num(arch + ".embedding_length"); ok && n > 0 {
			ff.Dimensions = Of(n, SourceInspect)
		}
	}
	if n, ok := num("split.count"); ok && n > 1 {
		ff.SplitCount = n
		if i, ok := num("split.no"); ok {
			ff.SplitIndex = i + 1
		}
	}
	return ff
}

// inspectSafetensors reads the JSON header: an 8-byte little-endian length,
// then the header. The precision is the dtype holding the most parameters.
func inspectSafetensors(path string, size int64) (FileFacts, bool) {
	f, err := os.Open(path)
	if err != nil {
		return FileFacts{}, false
	}
	defer f.Close()
	var lb [8]byte
	if _, err := io.ReadFull(f, lb[:]); err != nil {
		return FileFacts{}, false
	}
	n := binary.LittleEndian.Uint64(lb[:])
	if n < 2 || n > maxSafetensorsHd || int64(n)+8 > size {
		return FileFacts{}, false
	}
	hd := make([]byte, n)
	if _, err := io.ReadFull(f, hd); err != nil || hd[0] != '{' {
		return FileFacts{}, false
	}
	var header map[string]json.RawMessage
	if json.Unmarshal(hd, &header) != nil {
		return FileFacts{}, false
	}
	counts := map[string]int64{}
	for name, raw := range header {
		if name == "__metadata__" {
			continue
		}
		var t struct {
			Dtype string  `json:"dtype"`
			Shape []int64 `json:"shape"`
		}
		if json.Unmarshal(raw, &t) != nil || t.Dtype == "" {
			return FileFacts{}, false
		}
		p := int64(1)
		for _, d := range t.Shape {
			p *= d
		}
		counts[t.Dtype] += p
	}
	ff := FileFacts{Format: Of("safetensors", SourceInspect)}
	best, bestN := "", int64(-1)
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	if best != "" {
		ff.Precision = Of(best, SourceInspect)
	}
	// Sidecar config files name the architecture; they are data, never code.
	dir := filepath.Dir(path)
	if arch := sidecarArchitecture(dir); arch != "" {
		ff.Architecture = Of(arch, SourceInspect)
	}
	return ff, true
}

func sidecarArchitecture(dir string) string {
	read := func(name string) map[string]any {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(b) > 4<<20 {
			return nil
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			return nil
		}
		return m
	}
	if m := read("config.json"); m != nil {
		if t, ok := m["model_type"].(string); ok && t != "" {
			return t
		}
	}
	if m := read("model_index.json"); m != nil {
		if t, ok := m["_class_name"].(string); ok && t != "" {
			return t
		}
	}
	return ""
}

// Inspection is the result of checking a variant's files in dir.
type Inspection struct {
	Variant  Variant
	Missing  []string // required artifacts not present (or still partial)
	Problems []string // files that could not be identified
}

// Complete reports whether every required artifact is present.
func (i *Inspection) Complete() bool { return len(i.Missing) == 0 }

// InspectVariant merges what the files in dir say into a copy of v, and
// lists missing required files, including the other shards of a split GGUF.
func InspectVariant(v Variant, dir string) Inspection {
	out := Inspection{Variant: v}
	out.Variant.Artifacts = append([]Artifact(nil), v.Artifacts...)
	have := map[string]bool{}
	for _, a := range v.Artifacts {
		have[a.Filename] = true
	}
	var weights []FileFacts
	for _, a := range v.Artifacts {
		path := filepath.Join(dir, a.Filename)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || (a.SizeBytes > 0 && info.Size() != a.SizeBytes) {
			if a.Required {
				out.Missing = append(out.Missing, a.Filename)
			}
			continue
		}
		ff := InspectFile(path)
		if ff.Problem != "" {
			out.Problems = append(out.Problems, fmt.Sprintf("%s: %s", a.Filename, ff.Problem))
		}
		if ff.SplitCount > 1 {
			if m := shardNameRe.FindStringSubmatch(a.Filename); m != nil {
				for i := 1; i <= ff.SplitCount; i++ {
					name := fmt.Sprintf("%s-%05d-of-%05d.gguf", m[1], i, ff.SplitCount)
					if have[name] {
						continue
					}
					have[name] = true
					// A shard the record did not list joins the variant when it is
					// present, and is missing otherwise.
					src := a.Source
					src.URL = "" // another shard's URL cannot be derived
					if src.Path != "" {
						src.Path = strings.TrimPrefix(filepath.ToSlash(filepath.Join(filepath.Dir(src.Path), name)), "./")
					}
					out.Variant.Artifacts = append(out.Variant.Artifacts, Artifact{Filename: name, Role: RoleWeights, Required: true, Source: src})
					if st, err := os.Stat(filepath.Join(dir, name)); err != nil || !st.Mode().IsRegular() {
						out.Missing = append(out.Missing, name)
					}
				}
			}
		}
		if a.Role == RoleWeights && ff.Role != RoleProjector {
			weights = append(weights, ff)
		}
	}
	// The first readable weights file speaks for the variant (shard 1 of a
	// split set holds the metadata).
	sort.SliceStable(weights, func(i, j int) bool { return weights[i].SplitIndex < weights[j].SplitIndex })
	for _, ff := range weights {
		if ff.Problem != "" && !ff.Format.Known() {
			continue
		}
		out.Variant.Format = Merge(out.Variant.Format, ff.Format)
		out.Variant.Architecture = Merge(out.Variant.Architecture, ff.Architecture)
		out.Variant.Precision = Merge(out.Variant.Precision, ff.Precision)
		out.Variant.ContextLength = Merge(out.Variant.ContextLength, ff.ContextLength)
		out.Variant.Dimensions = Merge(out.Variant.Dimensions, ff.Dimensions)
		break
	}
	sort.Strings(out.Missing)
	return out
}
