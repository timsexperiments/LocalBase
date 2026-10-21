package memory

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GGUF header reading: only the keys that size the KV cache. Ported from
// gguf-metadata.ts with the same bounds against corrupt or hostile headers.

const (
	ggufMagic             = 0x46554747
	maxKVCount            = 1_000_000
	maxKeptArrayElements  = 4096
	maxKeptStringBytes    = 256
	maxParseWork          = 8_000_000
	swaUbatchTokens       = 512
	kvCellPadding         = 256
	hybridQwenFullAttnInt = 4
	recurrentStateBytes   = 4
	maxSSMKernel          = 64
	maxSSMSize            = 65536
	q8BlockSize           = 32
	maxBlockCount         = 4096
	maxHeadCount          = 4096
	maxHeadLength         = 65536
	maxTokens             = 1 << 31
)

var hybridQwen = map[string]bool{"qwen3next": true, "qwen35": true, "qwen35moe": true}

var unmodelledRecurrent = map[string]bool{
	"mamba": true, "mamba2": true, "rwkv6": true, "rwkv6qwen2": true, "rwkv7": true, "arwkv7": true,
	"jamba": true, "falcon-h1": true, "plamo2": true, "granitehybrid": true, "lfm2": true, "lfm2moe": true,
	"nemotron_h": true, "nemotron_h_moe": true, "kimi-linear": true, "deepseek4": true, "bailingmoe3": true,
	"kimi-k3": true, "glm5-next": true, "qwen4exp": true, "minimax-01": true,
}

type swaArch struct {
	period, defaultWindow, kvLayers int
}

var swaArchitectures = map[string]swaArch{
	"gpt-oss": {period: 2},
	"gemma2":  {period: 2, defaultWindow: 4096},
	"gemma3":  {period: 6},
	"gemma3n": {period: 5, kvLayers: 20},
	"cohere2": {period: 4},
}

// BytesPerElement of llama.cpp KV cache types.
var BytesPerElement = map[string]float64{
	"f32": 4, "f16": 2, "bf16": 2, "q8_0": 34.0 / 32, "q4_0": 18.0 / 32, "q4_1": 20.0 / 32,
	"q5_0": 22.0 / 32, "q5_1": 24.0 / 32, "iq4_nl": 18.0 / 32,
}

// Geometry is the per-model attention cache shape summed over KV layers.
type Geometry struct {
	Architecture          string
	BlockCount            int
	FullKVHeads           int
	SWAKVHeads            int
	SlidingWindow         int // 0 = none
	KeyLength             int
	ValueLength           int
	SWAKeyLength          int
	SWAValueLength        int
	Q8Compatible          bool
	RecurrentBytesPerSlot int64
	ContextLength         int // 0 = unknown
	DecisionType          string
}

var keptSuffixes = []string{
	".block_count", ".context_length", ".embedding_length", ".attention.head_count",
	".attention.head_count_kv", ".attention.key_length", ".attention.value_length",
	".attention.sliding_window", ".attention.sliding_window_pattern", ".attention.key_length_mla",
	".attention.value_length_mla", ".attention.key_length_swa", ".attention.value_length_swa",
	".attention.shared_kv_layers", ".attention.recurrent_layers", ".full_attention_interval",
	".nextn_predict_layers", ".ssm.conv_kernel", ".ssm.inner_size", ".ssm.state_size",
	".ssm.group_count", ".decision.type",
}

func keepKey(k string) bool {
	if k == "general.architecture" {
		return true
	}
	for _, s := range keptSuffixes {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

var shardRe = regexp.MustCompile(`^(.*)-(\d{5})-of-(\d{5})\.gguf$`)

// MetadataPath maps any shard of a split model to shard 00001, which holds the metadata.
func MetadataPath(path string) string {
	m := shardRe.FindStringSubmatch(filepath.Base(path))
	if m == nil {
		return path
	}
	return filepath.Join(filepath.Dir(path), m[1]+"-00001-of-"+m[3]+".gguf")
}

type ggufReader struct {
	r    *bufio.Reader
	work int
}

func (g *ggufReader) spend(n int) error {
	g.work += n
	if g.work > maxParseWork {
		return errors.New("GGUF header too large")
	}
	return nil
}

func (g *ggufReader) u32() (uint32, error) {
	var b [4]byte
	_, err := io.ReadFull(g.r, b[:])
	return binary.LittleEndian.Uint32(b[:]), err
}

func (g *ggufReader) u64() (uint64, error) {
	var b [8]byte
	_, err := io.ReadFull(g.r, b[:])
	return binary.LittleEndian.Uint64(b[:]), err
}

func (g *ggufReader) str(keep bool, maxBytes int) (string, bool, error) {
	n, err := g.u64()
	if err != nil {
		return "", false, err
	}
	if n > 1<<30 {
		return "", false, errors.New("implausible GGUF string")
	}
	if !keep || n > uint64(maxBytes) {
		_, err := g.r.Discard(int(n))
		return "", false, err
	}
	b := make([]byte, n)
	_, err = io.ReadFull(g.r, b)
	return string(b), true, err
}

var fixedSizes = map[uint32]int{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}

// scalar reads one fixed-size value as float64 (NaN for 64-bit ints past 2^53).
func (g *ggufReader) scalar(t uint32) (float64, error) {
	size := fixedSizes[t]
	b := make([]byte, size)
	if _, err := io.ReadFull(g.r, b); err != nil {
		return 0, err
	}
	switch t {
	case 0:
		return float64(b[0]), nil
	case 1:
		return float64(int8(b[0])), nil
	case 2:
		return float64(binary.LittleEndian.Uint16(b)), nil
	case 3:
		return float64(int16(binary.LittleEndian.Uint16(b))), nil
	case 4:
		return float64(binary.LittleEndian.Uint32(b)), nil
	case 5:
		return float64(int32(binary.LittleEndian.Uint32(b))), nil
	case 6:
		return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))), nil
	case 7:
		if b[0] != 0 {
			return 1, nil
		}
		return 0, nil
	case 10:
		v := binary.LittleEndian.Uint64(b)
		if v > 1<<53 {
			return math.NaN(), nil
		}
		return float64(v), nil
	case 11:
		v := int64(binary.LittleEndian.Uint64(b))
		if v > 1<<53 || v < -(1<<53) {
			return math.NaN(), nil
		}
		return float64(v), nil
	case 12:
		return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
	}
	return 0, fmt.Errorf("unknown GGUF type %d", t)
}

// value is a kept scalar (float64), string, or []float64.
func (g *ggufReader) value(t uint32, keep bool, maxStr int) (any, error) {
	switch {
	case t == 8:
		s, ok, err := g.str(keep, maxStr)
		if !ok {
			return nil, err
		}
		return s, err
	case t == 9:
		et, err := g.u32()
		if err != nil {
			return nil, err
		}
		n, err := g.u64()
		if err != nil {
			return nil, err
		}
		if err := g.spend(int(min(n, maxParseWork+1))); err != nil {
			return nil, err
		}
		size, fixed := fixedSizes[et]
		keepArr := keep && fixed && n <= maxKeptArrayElements
		if !keepArr {
			if fixed {
				_, err := g.r.Discard(int(n) * size)
				return nil, err
			}
			for i := uint64(0); i < n; i++ {
				if _, err := g.value(et, false, 0); err != nil {
					return nil, err
				}
			}
			return nil, nil
		}
		out := make([]float64, n)
		for i := range out {
			if out[i], err = g.scalar(et); err != nil {
				return nil, err
			}
		}
		return out, nil
	default:
		if _, ok := fixedSizes[t]; !ok {
			return nil, fmt.Errorf("unknown GGUF type %d", t)
		}
		v, err := g.scalar(t)
		if !keep {
			return nil, err
		}
		return v, err
	}
}

// ReadGGUFKeys reads the geometry-relevant header keys of a GGUF file.
func ReadGGUFKeys(path string) (map[string]any, error) {
	return readKeys(path, keepKey, func(string) int { return maxKeptStringBytes })
}

// readKeys reads the header keys keep selects; maxStr bounds each kept string.
func readKeys(path string, keep func(string) bool, maxStr func(string) int) (map[string]any, error) {
	f, err := os.Open(MetadataPath(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	g := &ggufReader{r: bufio.NewReaderSize(f, 1<<20)}
	if magic, err := g.u32(); err != nil || magic != ggufMagic {
		return nil, errors.New("not a GGUF file")
	}
	if v, _ := g.u32(); v != 2 && v != 3 {
		return nil, fmt.Errorf("unsupported GGUF version %d", v)
	}
	if _, err := g.u64(); err != nil {
		return nil, err
	}
	count, err := g.u64()
	if err != nil || count > maxKVCount {
		return nil, errors.New("implausible GGUF key count")
	}
	values := map[string]any{}
	for i := uint64(0); i < count; i++ {
		if err := g.spend(1); err != nil {
			return nil, err
		}
		key, _, err := g.str(true, maxKeptStringBytes)
		if err != nil {
			return nil, err
		}
		t, err := g.u32()
		if err != nil {
			return nil, err
		}
		k := keep(key)
		v, err := g.value(t, k, maxStr(key))
		if err != nil {
			return nil, err
		}
		if k && v != nil {
			values[key] = v
		}
	}
	return values, nil
}

// ReadGeometry returns nil on any failure, like the TS reader.
func ReadGeometry(path string) *Geometry {
	values, err := ReadGGUFKeys(path)
	if err != nil {
		return nil
	}
	return GeometryFromMetadata(values)
}

func bounded(v any, max int) int {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || f <= 0 || f > float64(max) {
		return 0
	}
	return int(f)
}

// optionalInt: (value, present, valid).
func optionalInt(v any, lo, hi int) (int, bool, bool) {
	if v == nil {
		return 0, false, true
	}
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || f < float64(lo) || f > float64(hi) {
		return 0, true, false
	}
	return int(f), true, true
}

// layerValues: per-layer ints from a scalar or a per-layer array. (nil, true)
// when absent; (nil, false) when malformed.
func layerValues(v any, blocks int) ([]int, bool) {
	if v == nil {
		return nil, true
	}
	valid := func(f float64) bool { return f == math.Trunc(f) && f >= 0 && f <= maxHeadCount }
	if arr, ok := v.([]float64); ok {
		if len(arr) != blocks {
			return nil, false
		}
		out := make([]int, blocks)
		for i, f := range arr {
			if !valid(f) {
				return nil, false
			}
			out[i] = int(f)
		}
		return out, true
	}
	f, ok := v.(float64)
	if !ok || !valid(f) {
		return nil, false
	}
	out := make([]int, blocks)
	for i := range out {
		out[i] = int(f)
	}
	return out, true
}

// GeometryFromMetadata ports kvGeometryFromMetadata. Nil when unknown or malformed.
func GeometryFromMetadata(values map[string]any) *Geometry {
	arch, _ := values["general.architecture"].(string)
	if arch == "" || unmodelledRecurrent[arch] {
		return nil
	}
	get := func(s string) any { return values[arch+"."+s] }
	blocks := bounded(get("block_count"), maxBlockCount)
	if blocks == 0 {
		return nil
	}
	headCounts, ok1 := layerValues(get("attention.head_count"), blocks)
	kvHeadCounts, ok2 := layerValues(get("attention.head_count_kv"), blocks)
	if headCounts == nil || !ok1 || !ok2 {
		return nil
	}
	headsFor := func(l int) int {
		if kvHeadCounts != nil {
			return kvHeadCounts[l]
		}
		return headCounts[l]
	}
	embd := bounded(get("embedding_length"), maxTokens)
	fallback := 0
	if embd > 0 && headCounts[0] > 0 {
		fallback = embd / headCounts[0]
	}
	keyLen := bounded(get("attention.key_length"), maxHeadLength)
	if keyLen == 0 {
		keyLen = fallback
	}
	valLen := bounded(get("attention.value_length"), maxHeadLength)
	if valLen == 0 {
		valLen = fallback
	}
	if valLen == 0 {
		valLen = keyLen
	}
	if keyLen == 0 || valLen == 0 || keyLen > maxHeadLength {
		return nil
	}
	kMLA := bounded(get("attention.key_length_mla"), maxHeadLength)
	vMLA := bounded(get("attention.value_length_mla"), maxHeadLength)
	mla := kMLA != 0 && vMLA != 0

	gemma4 := arch == "gemma4"
	isHybrid := hybridQwen[arch]
	spec, hasSWA := swaArchitectures[arch]
	window := 0
	if hasSWA || gemma4 {
		window = bounded(get("attention.sliding_window"), maxTokens)
		if window == 0 {
			window = spec.defaultWindow
		}
	}
	swaKey, swaVal := keyLen, valLen
	if gemma4 {
		sk := bounded(get("attention.key_length_swa"), maxHeadLength)
		sv := bounded(get("attention.value_length_swa"), maxHeadLength)
		if window == 0 || sk == 0 || sv == 0 {
			return nil
		}
		swaKey, swaVal = sk, sv
	}
	nextn := 0
	if isHybrid {
		n, _, ok := optionalInt(get("nextn_predict_layers"), 0, blocks-1)
		if !ok {
			return nil
		}
		nextn = n
	}
	attnLayers := blocks - nextn

	// Which layers are sliding-window.
	var isSWA func(int) bool
	pattern := get("attention.sliding_window_pattern")
	switch {
	case gemma4:
		if arr, ok := pattern.([]float64); ok {
			if len(arr) != blocks {
				return nil
			}
			isSWA = func(l int) bool { return arr[l] != 0 }
		} else {
			isSWA = func(int) bool { return false }
		}
	case window == 0:
		isSWA = func(int) bool { return false }
	default:
		period := spec.period
		if f, ok := pattern.(float64); ok && f == math.Trunc(f) && f >= 0 && f <= maxBlockCount {
			period = int(f)
		}
		isSWA = func(l int) bool { return period == 0 || (l+1)%period != 0 }
	}

	kvLayers := attnLayers
	if spec.kvLayers > 0 && spec.kvLayers < kvLayers {
		kvLayers = spec.kvLayers
	}
	if gemma4 {
		shared, _, ok := optionalInt(get("attention.shared_kv_layers"), 0, blocks)
		if !ok || blocks-shared < 1 {
			return nil
		}
		kvLayers = blocks - shared
	}

	isRecurrent := func(int) bool { return false }
	var recurrentBytes int64
	if isHybrid {
		var rec func(int) bool
		if mask := get("attention.recurrent_layers"); mask != nil {
			arr, ok := mask.([]float64)
			if !ok || len(arr) != blocks {
				return nil
			}
			rec = func(l int) bool { return arr[l] != 0 }
		} else {
			interval, present, ok := optionalInt(get("full_attention_interval"), 1, maxBlockCount)
			if !ok {
				return nil
			}
			if !present {
				interval = hybridQwenFullAttnInt
			}
			rec = func(l int) bool { return (l+1)%interval != 0 }
		}
		kernel := bounded(get("ssm.conv_kernel"), maxSSMKernel)
		inner := bounded(get("ssm.inner_size"), maxSSMSize)
		state := bounded(get("ssm.state_size"), maxSSMSize)
		groups, present, ok := optionalInt(get("ssm.group_count"), 0, maxSSMSize)
		if kernel == 0 || inner == 0 || state == 0 || !present || !ok {
			return nil
		}
		isRecurrent = func(l int) bool { return l < attnLayers && rec(l) }
		n := 0
		for l := 0; l < attnLayers; l++ {
			if isRecurrent(l) {
				n++
			}
		}
		conv := int64(kernel-1) * int64(inner+2*groups*state)
		st := int64(state) * int64(inner)
		recurrentBytes = int64(n) * (conv + st) * recurrentStateBytes
	}

	g := &Geometry{Architecture: arch, BlockCount: blocks, SlidingWindow: window,
		KeyLength: keyLen, ValueLength: valLen, SWAKeyLength: swaKey, SWAValueLength: swaVal,
		RecurrentBytesPerSlot: recurrentBytes, ContextLength: bounded(get("context_length"), maxTokens)}
	anySWA := false
	for l := 0; l < blocks; l++ {
		if isSWA(l) {
			anySWA = true
		}
	}
	for l := 0; l < kvLayers; l++ {
		h := headsFor(l)
		if h <= 0 || isRecurrent(l) {
			continue
		}
		if isSWA(l) {
			g.SWAKVHeads += h
		} else {
			g.FullKVHeads += h
		}
	}
	dims := []int{keyLen, valLen}
	if anySWA {
		dims = append(dims, swaKey, swaVal)
	}
	if mla {
		dims = append(dims, kMLA, vMLA)
	}
	g.Q8Compatible = true
	for _, d := range dims {
		if d%q8BlockSize != 0 {
			g.Q8Compatible = false
		}
	}
	if dt, ok := get("decision.type").(string); ok && regexp.MustCompile(`^[a-z0-9_-]{1,32}$`).MatchString(dt) {
		g.DecisionType = dt
		g.FullKVHeads, g.SWAKVHeads, g.RecurrentBytesPerSlot = 0, 0, 0
	}
	if mla {
		g.ValueLength, g.SWAValueLength = 0, 0
	}
	return g
}

func padCells(n int) int { return (n + kvCellPadding - 1) / kvCellPadding * kvCellPadding }

// KVCacheBytes ports kvCacheBytes: total context split across slots, padded.
func KVCacheBytes(g *Geometry, ctxTokens, slots int, typeK, typeV string) int64 {
	bk, bv := BytesPerElement[typeK], BytesPerElement[typeV]
	if slots < 1 {
		slots = 1
	}
	cells := padCells(padCells(ctxTokens) / slots)
	full := float64(slots) * float64(cells) * float64(g.FullKVHeads) * (float64(g.KeyLength)*bk + float64(g.ValueLength)*bv)
	swaCells := 0
	if g.SlidingWindow != 0 {
		swaCells = padCells(min(cells, g.SlidingWindow+swaUbatchTokens))
	}
	swa := float64(slots) * float64(swaCells) * float64(g.SWAKVHeads) * (float64(g.SWAKeyLength)*bk + float64(g.SWAValueLength)*bv)
	return int64(math.Ceil(full + swa + float64(slots)*float64(g.RecurrentBytesPerSlot)))
}

const (
	chatTemplateKey      = "tokenizer.chat_template"
	maxChatTemplateBytes = 256 * 1024
)

// CapabilityHeader is what a GGUF header says about a model's capabilities.
type CapabilityHeader struct {
	Architecture     string
	ContextLength    int // 0 = unknown
	PoolingType      *int
	ExpertCount      int
	ChatTemplate     string // default and named variants, joined
	DecisionType     string
	HasVisionEncoder bool
	HasAudioEncoder  bool
}

var decisionTypeRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// ReadCapabilityHeader returns nil on any failure.
func ReadCapabilityHeader(path string) *CapabilityHeader {
	values, err := readKeys(path, func(k string) bool {
		return k == "general.architecture" || k == chatTemplateKey || strings.HasPrefix(k, chatTemplateKey+".") ||
			strings.HasSuffix(k, ".context_length") || strings.HasSuffix(k, ".pooling_type") ||
			strings.HasSuffix(k, ".expert_count") || strings.HasSuffix(k, ".decision.type") ||
			k == "clip.has_vision_encoder" || k == "clip.has_audio_encoder"
	}, func(k string) int {
		if strings.HasPrefix(k, chatTemplateKey) {
			return maxChatTemplateBytes
		}
		return maxKeptStringBytes
	})
	if err != nil {
		return nil
	}
	h := &CapabilityHeader{}
	h.Architecture, _ = values["general.architecture"].(string)
	if a := h.Architecture; a != "" {
		h.ContextLength = bounded(values[a+".context_length"], maxTokens)
		if p, ok := values[a+".pooling_type"].(float64); ok && p == math.Trunc(p) {
			n := int(p)
			h.PoolingType = &n
		}
		if e, ok := values[a+".expert_count"].(float64); ok && e == math.Trunc(e) && e > 0 {
			h.ExpertCount = int(e)
		}
		if d, ok := values[a+".decision.type"].(string); ok && decisionTypeRe.MatchString(d) {
			h.DecisionType = d
		}
	}
	var templates []string
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := values[k].(string); ok && s != "" && (k == chatTemplateKey || strings.HasPrefix(k, chatTemplateKey+".")) {
			templates = append(templates, s)
		}
	}
	h.ChatTemplate = strings.Join(templates, "\n")
	h.HasVisionEncoder = values["clip.has_vision_encoder"] == 1.0
	h.HasAudioEncoder = values["clip.has_audio_encoder"] == 1.0
	return h
}
