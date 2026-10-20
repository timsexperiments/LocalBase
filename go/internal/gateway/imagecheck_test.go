package gateway

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/color/palette"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// rawPNG assembles a PNG from an IHDR and raw (filtered) scanlines.
func rawPNG(w, h uint32, depth, color, interlace byte, raw []byte, extra ...[]byte) []byte {
	chunk := func(typ string, data []byte) []byte {
		var c bytes.Buffer
		binary.Write(&c, binary.BigEndian, uint32(len(data)))
		c.WriteString(typ)
		c.Write(data)
		binary.Write(&c, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
		return c.Bytes()
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9], ihdr[12] = depth, color, interlace
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(raw)
	zw.Close()
	out := append([]byte{}, pngSignature...)
	out = append(out, chunk("IHDR", ihdr)...)
	for _, e := range extra {
		out = append(out, e...)
	}
	out = append(out, chunk("IDAT", z.Bytes())...)
	return append(out, chunk("IEND", nil)...)
}

func TestValidatePNG(t *testing.T) {
	rgba := image.NewRGBA(image.Rect(0, 0, 7, 3))
	rgba.Set(1, 1, color.RGBA{1, 2, 3, 4})
	pal := image.NewPaletted(image.Rect(0, 0, 5, 5), palette.Plan9)
	gray16 := image.NewGray16(image.Rect(0, 0, 3, 9))
	for name, b := range map[string][]byte{"rgba": encodePNG(t, rgba), "paletted": encodePNG(t, pal), "gray16": encodePNG(t, gray16),
		// 3x2 gray 8-bit, interlaced: Adam7 passes 1, 4 and 6 hold one pixel, pass 7 three.
		"interlaced": rawPNG(3, 2, 8, 0, 1, []byte{0, 1, 0, 2, 0, 3, 0, 4, 5, 6}),
	} {
		if d := validatePNG(b); d == nil {
			t.Errorf("%s: rejected", name)
		}
	}
	if d := validatePNG(encodePNG(t, rgba)); d == nil || d.width != 7 || d.height != 3 {
		t.Errorf("dimensions %+v", d)
	}
	good := rawPNG(2, 1, 8, 0, 0, []byte{0, 1, 2})
	for name, b := range map[string][]byte{
		"trailing data":    append(append([]byte{}, good...), 0),
		"short raw":        rawPNG(2, 2, 8, 0, 0, []byte{0, 1, 2}),
		"long raw":         rawPNG(2, 1, 8, 0, 0, []byte{0, 1, 2, 3}),
		"bad filter":       rawPNG(2, 1, 8, 0, 0, []byte{5, 1, 2}),
		"bad depth":        rawPNG(2, 1, 3, 0, 0, []byte{0, 1}),
		"bad interlace":    rawPNG(2, 1, 8, 0, 2, []byte{0, 1, 2}),
		"too wide":         rawPNG(2049, 1, 8, 0, 0, make([]byte, 2050)),
		"no IEND":          good[:len(good)-12],
		"palette missing":  rawPNG(2, 1, 8, 3, 0, []byte{0, 1, 2}),
		"critical unknown": rawPNG(2, 1, 8, 0, 0, []byte{0, 1, 2}, []byte{0, 0, 0, 0, 'A', 'B', 'C', 'D', 0, 0, 0, 0}),
		"signature only":   pngSignature,
	} {
		if validatePNG(b) != nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An ancillary chunk (lowercase first letter) is allowed.
	if validatePNG(rawPNG(2, 1, 8, 0, 0, []byte{0, 1, 2}, []byte{0, 0, 0, 0, 't', 'E', 'X', 't', 0, 0, 0, 0})) == nil {
		t.Error("ancillary chunk rejected")
	}
	// Corrupt zlib data.
	broken := append([]byte{}, good...)
	broken[8+25+8+3] ^= 0xff
	if validatePNG(broken) != nil {
		t.Error("corrupt IDAT accepted")
	}
}

func TestValidateJPEG(t *testing.T) {
	var b bytes.Buffer
	jpeg.Encode(&b, image.NewGray(image.Rect(0, 0, 9, 4)), nil)
	good := b.Bytes()
	if d := validateJPEG(good); d == nil || d.width != 9 || d.height != 4 {
		t.Fatalf("valid jpeg: %+v", d)
	}
	for name, x := range map[string][]byte{
		"truncated":     good[:len(good)-2],
		"trailing":      append(append([]byte{}, good...), 0),
		"no SOI":        good[2:],
		"just SOI+EOI":  {0xff, 0xd8, 0xff, 0xd9},
		"short segment": {0xff, 0xd8, 0xff, 0xe0, 0x00},
	} {
		if validateJPEG(x) != nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if imageMimeFromMagic(good) != "image/jpeg" || imageMimeFromMagic([]byte("GIF89a")) != "" {
		t.Error("magic")
	}
}

func TestFileTypeOf(t *testing.T) {
	for name, want := range map[string]string{"a.png": "image/png", "x.PNG": "image/png", "x.jpe": "image/jpeg", "a.b.JPG": "image/jpeg",
		"image": "", ".png": "", "x.": "", "": ""} {
		if got := fileTypeOf(name); got != want {
			t.Errorf("%q: %q, want %q", name, got, want)
		}
	}
	if fileTypeOf("x.gif") == "" || fileTypeOf("in.bin") == "" {
		t.Error("known extensions should have a type")
	}
}

// TestPromptWithTuning matches TS promptWithImageTuning output exactly
// (strings recorded from the TS function).
func TestPromptWithTuning(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	i := func(v int64) *int64 { return &v }
	cases := []struct {
		prompt string
		tuning imageTuning
		lora   *imageLora
		want   string
	}{
		{"a cat", imageTuning{}, nil, "a cat"},
		{"a cat", imageTuning{negativePrompt: `blur <b> & "q"`, seed: i(42), steps: i(6), cfgScale: f(7.5), strength: f(0.25)},
			&imageLora{"/m/lora.safetensors", 0.8},
			`a cat <sd_cpp_extra_args>{"lora":[{"path":"/m/lora.safetensors","multiplier":0.8,"is_high_noise":false}],"negative_prompt":"blur <b> & \"q\"","seed":42,"strength":0.25,"sample_params":{"sample_steps":6,"guidance":{"txt_cfg":7.5}}}</sd_cpp_extra_args>`},
		{"x", imageTuning{cfgScale: f(1e21), seed: i(0)}, nil, `x <sd_cpp_extra_args>{"seed":0,"sample_params":{"guidance":{"txt_cfg":1e+21}}}</sd_cpp_extra_args>`},
		{"x", imageTuning{steps: i(3)}, nil, `x <sd_cpp_extra_args>{"sample_params":{"sample_steps":3}}</sd_cpp_extra_args>`},
	}
	for _, c := range cases {
		if got := promptWithTuning(c.prompt, c.tuning, c.lora); got != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}
