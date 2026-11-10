package infer

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// pngWith is a PNG holding the given text chunks before its image data, in
// the layout sd-server writes.
func pngWith(chunks ...[2]string) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	add := func(kind string, data []byte) {
		binary.Write(&b, binary.BigEndian, uint32(len(data)))
		body := append([]byte(kind), data...)
		b.Write(body)
		binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(body))
	}
	add("IHDR", make([]byte, 13))
	for _, c := range chunks {
		add("tEXt", []byte(c[0]+"\x00"+c[1]))
	}
	add("IDAT", []byte("x"))
	add("IEND", nil)
	return b.Bytes()
}

// Real sd-server text (the 2026-11-01 run on the pinned build), shortened.
const sdParameters = "a red apple on a table \nSteps: 4, CFG scale: 7.000000, Guidance: 3.500000, Eta: inf, Seed: 43, Size: 256x256, " +
	`Model: DreamShaper_8_pruned.safetensors, RNG: cuda, Sampler: NONE, Version: stable-diffusion.cpp, SDCPP: {"seed":43}`

func TestImageSeedFromPNG(t *testing.T) {
	for name, c := range map[string]struct {
		png  []byte
		seed int64
		ok   bool
	}{
		"sd-server": {pngWith([2]string{"parameters", sdParameters}), 43, true},
		// The prompt comes first, so text in it cannot pose as the seed.
		"prompt says otherwise": {pngWith([2]string{"parameters", "draw Seed: 99, Size: 1x1 on a sign\n" + sdParameters}), 43, true},
		"other text first":      {pngWith([2]string{"Software", "x"}, [2]string{"parameters", sdParameters}), 43, true},
		"no parameters":         {pngWith([2]string{"Software", "Seed: 7, Size: 1x1"}), 0, false},
		"no seed in them":       {pngWith([2]string{"parameters", "a prompt"}), 0, false},
		"not a PNG":             {[]byte("\xff\xd8\xff\xe0 jpeg"), 0, false},
		"cut short":             {pngWith([2]string{"parameters", sdParameters})[:40], 0, false},
		"empty":                 {nil, 0, false},
	} {
		if seed, ok := imageSeedFromPNG(c.png); seed != c.seed || ok != c.ok {
			t.Errorf("%s: %d %v", name, seed, ok)
		}
	}
}

// The file wins; a JPEG falls back to the requested seed + i; with neither,
// the field stays out rather than guessing.
func TestAnnotateImageSeeds(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString
	jpeg := enc([]byte("\xff\xd8\xff\xe0 jpeg"))
	response := func(images ...string) map[string]any {
		var data []any
		for _, b := range images {
			data = append(data, map[string]any{"b64_json": b})
		}
		return map[string]any{"data": data}
	}
	seeds := func(o map[string]any) []any {
		var out []any
		for _, d := range o["data"].([]any) {
			out = append(out, d.(map[string]any)["seed"])
		}
		return out
	}
	req := int64(7)
	o := response(enc(pngWith([2]string{"parameters", sdParameters})))
	annotateImageSeeds(o, &req)
	if got := seeds(o); got[0] != int64(43) {
		t.Errorf("the file should win over the request: %v", got)
	}
	o = response(jpeg, jpeg)
	annotateImageSeeds(o, &req)
	if got := seeds(o); got[0] != int64(7) || got[1] != int64(8) {
		t.Errorf("jpeg with a seed: %v", got)
	}
	o = response(jpeg)
	annotateImageSeeds(o, nil)
	if got := seeds(o); got[0] != nil {
		t.Errorf("jpeg with no seed: %v", got)
	}
	annotateImageSeeds(map[string]any{"data": []any{"not an object"}}, &req) // no panic
}
