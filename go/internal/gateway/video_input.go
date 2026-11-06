package gateway

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"io"
	"math"
	"regexp"
)

// Native video input validation (TS video-input.ts videoGenerationInputSchema):
// the input after catalog qualification. Speech conditioning accepts only
// canonical inline PCM16 or normalized float32 WAV and non-interlaced 8-bit
// RGB or RGBA PNG; image conditioning the same PNG.

const (
	maxVideoWAVBytes = 8 << 20
	maxVideoPNGBytes = 4 << 20
)

var (
	maxBase64WAVLength = (maxVideoWAVBytes + 2) / 3 * 4
	maxBase64PNGLength = (maxVideoPNGBytes + 2) / 3 * 4
	canonicalBase64    = regexp.MustCompile(`^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`)
)

// validVideoInput reports whether in passes videoGenerationInputSchema.
func validVideoInput(in map[string]any) bool {
	common := []string{"kind", "prompt", "negativePrompt", "width", "height", "videoFrames", "fps", "seed", "outputFormat", "generation"}
	kind, _ := in["kind"].(string)
	var keys []string
	switch kind {
	case "text":
		keys = common
	case "speech":
		keys = append(common, "portrait", "audio")
	case "image":
		keys = append(common, "image")
	default:
		return false
	}
	for k := range in {
		if !contains(keys, k) {
			return false
		}
	}
	if !jsString(in["prompt"], 1, 16_384) || !optional(in, "negativePrompt", func(v any) bool { return jsString(v, 0, 16_384) }) {
		return false
	}
	canvasRequired := kind != "text"
	for _, f := range []struct {
		key string
		max float64
	}{{"width", 16_384}, {"height", 16_384}, {"videoFrames", 4_097}, {"fps", 240}} {
		if _, present := in[f.key]; !present {
			if canvasRequired {
				return false
			}
			continue
		}
		if n, ok := jsInt(in[f.key]); !ok || n <= 0 || n > f.max {
			return false
		}
	}
	if !optional(in, "seed", func(v any) bool { _, ok := jsInt(v); return ok }) ||
		!optional(in, "outputFormat", func(v any) bool { s, _ := v.(string); return s == "webm" || s == "webp" || s == "avi" }) ||
		!optional(in, "generation", validVideoGeneration) {
		return false
	}
	switch kind {
	case "image":
		img, ok := inlineMedia(in["image"], "png", maxBase64PNGLength)
		if !ok {
			return false
		}
		png := decodeCanonicalBase64(img)
		if png == nil || len(png) > maxVideoPNGBytes {
			return false
		}
		w, h, ok := videoPNG(png)
		return ok && float64(w) == num(in["width"]) && float64(h) == num(in["height"])
	case "speech":
		portrait, ok1 := inlineMedia(in["portrait"], "png", maxBase64PNGLength)
		audio, ok2 := inlineMedia(in["audio"], "wav", maxBase64WAVLength)
		if !ok1 || !ok2 {
			return false
		}
		valid := true
		png := decodeCanonicalBase64(portrait)
		if png == nil || len(png) > maxVideoPNGBytes {
			valid = false
		} else if w, h, ok := videoPNG(png); !ok || float64(w) != num(in["width"]) || float64(h) != num(in["height"]) {
			valid = false
		}
		wav := decodeCanonicalBase64(audio)
		if wav == nil || len(wav) > maxVideoWAVBytes {
			return false
		}
		rate, frames, ok := parseVideoWAV(wav)
		if !ok {
			return false
		}
		if float64(frames)*num(in["fps"]) > float64(rate)*num(in["videoFrames"]) {
			return false
		}
		return valid
	}
	return true
}

func validVideoGeneration(v any) bool {
	g, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for k := range g {
		if !contains([]string{"sampler", "scheduler", "steps", "cfgScale", "flowShift"}, k) {
			return false
		}
	}
	scheduler, _ := g["scheduler"].(string)
	steps, okSteps := jsInt(g["steps"])
	cfg, okCfg := number(g["cfgScale"])
	shift, okShift := number(g["flowShift"])
	return g["sampler"] == "euler" && (scheduler == "discrete" || scheduler == "lcm") &&
		okSteps && steps > 0 && steps <= 1_000 && okCfg && cfg > 0 && cfg <= 100 && okShift && shift > 0 && shift <= 100
}

// inlineMedia reads a strict {format, data} object.
func inlineMedia(v any, format string, maxLength int) (string, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 2 || m["format"] != format {
		return "", false
	}
	data, ok := m["data"].(string)
	return data, ok && jsString(data, 1, maxLength)
}

func optional(obj map[string]any, key string, valid func(any) bool) bool {
	v, present := obj[key]
	return !present || valid(v)
}

// jsString reports a string whose UTF-16 length is within [lo, hi].
func jsString(v any, lo, hi int) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	n := utf16Length(s)
	return n >= lo && n <= hi
}

// jsInt reads a Zod int: a finite whole number within the safe range.
func jsInt(v any) (float64, bool) {
	f, ok := number(v)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > 1<<53-1 {
		return 0, false
	}
	return f, true
}

func num(v any) float64 {
	f, _ := number(v)
	return f
}

func decodeCanonicalBase64(s string) []byte {
	if s == "" || len(s)%4 != 0 || !canonicalBase64.MatchString(s) {
		return nil
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || base64.StdEncoding.EncodeToString(b) != s {
		return nil
	}
	return b
}

// videoPNG is the stricter PNG policy of video conditioning on top of the
// image-edit checks: 8-bit RGB or RGBA, no interlace, valid CRCs, no unknown
// critical chunks, ancillary chunks only before the image data, and exactly
// the scanlines the header declares with valid filter bytes.
func videoPNG(b []byte) (int, int, bool) {
	if validatePNG(b) == nil || len(b) < 33 || !bytes.Equal(b[:8], pngSignature) {
		return 0, 0, false
	}
	be := binary.BigEndian
	if be.Uint32(b[8:]) != 13 || string(b[12:16]) != "IHDR" {
		return 0, 0, false
	}
	width, height := int(be.Uint32(b[16:])), int(be.Uint32(b[20:]))
	bitDepth, colorType := b[24], b[25]
	if width == 0 || height == 0 || bitDepth != 8 || (colorType != 2 && colorType != 6) || b[26] != 0 || b[27] != 0 || b[28] != 0 {
		return 0, 0, false
	}
	channels := 3
	if colorType == 6 {
		channels = 4
	}
	rowBytes := width * channels
	if rowBytes*height > maxVideoPNGBytes {
		return 0, 0, false
	}
	inflated := (rowBytes + 1) * height
	offset := 8
	sawEnd, sawHeader, dataEnded := false, false, false
	var data []byte
	for offset+12 <= len(b) {
		length := int(be.Uint32(b[offset:]))
		next := offset + 12 + length
		if next > len(b) || next < offset {
			return 0, 0, false
		}
		typ := string(b[offset+4 : offset+8])
		if crc32.ChecksumIEEE(b[offset+4:offset+8+length]) != be.Uint32(b[offset+8+length:]) {
			return 0, 0, false
		}
		switch typ {
		case "IHDR":
			if sawHeader || offset != 8 || length != 13 {
				return 0, 0, false
			}
			sawHeader = true
		case "IDAT":
			if !sawHeader || sawEnd || dataEnded {
				return 0, 0, false
			}
			if len(data)+length > maxVideoPNGBytes {
				return 0, 0, false
			}
			data = append(data, b[offset+8:offset+8+length]...)
		case "IEND":
			if length != 0 || next != len(b) {
				return 0, 0, false
			}
			sawEnd = true
		default:
			if len(data) > 0 {
				dataEnded = true
			}
			if b[offset+4] >= 'A' && b[offset+4] <= 'Z' {
				return 0, 0, false
			}
		}
		if sawEnd {
			break
		}
		offset = next
	}
	if !sawEnd || !sawHeader || len(data) == 0 {
		return 0, 0, false
	}
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false
	}
	out, err := io.ReadAll(io.LimitReader(zr, int64(inflated)+1))
	if (err != nil && err != io.ErrUnexpectedEOF) || len(out) != inflated {
		return 0, 0, false
	}
	for i := 0; i < len(out); i += rowBytes + 1 {
		if out[i] > 4 {
			return 0, 0, false
		}
	}
	return width, height, true
}

// parseVideoWAV reads a canonical PCM16 or normalized float32 WAV.
func parseVideoWAV(b []byte) (rate, frames int, ok bool) {
	if len(b) < 44 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, 0, false
	}
	le := binary.LittleEndian
	if int(le.Uint32(b[4:]))+8 != len(b) {
		return 0, 0, false
	}
	var encoding, channels, blockAlign, bits int
	var sampleRate, byteRate int
	var haveFormat bool
	var samples []byte
	offset := 12
	for offset+8 <= len(b) {
		id := string(b[offset : offset+4])
		size := int(le.Uint32(b[offset+4:]))
		start := offset + 8
		end := start + size
		if end > len(b) || end < start {
			return 0, 0, false
		}
		if id == "fmt " && !haveFormat {
			if size < 16 {
				return 0, 0, false
			}
			haveFormat = true
			encoding, channels = int(le.Uint16(b[start:])), int(le.Uint16(b[start+2:]))
			sampleRate, byteRate = int(le.Uint32(b[start+4:])), int(le.Uint32(b[start+8:]))
			blockAlign, bits = int(le.Uint16(b[start+12:])), int(le.Uint16(b[start+14:]))
		} else if id == "data" && samples == nil {
			samples = b[start:end]
		}
		offset = end + size%2
	}
	if offset != len(b) || !haveFormat || samples == nil {
		return 0, 0, false
	}
	if (encoding != 1 && encoding != 3) || (encoding == 1 && bits != 16) || (encoding == 3 && bits != 32) ||
		(channels != 1 && channels != 2) || sampleRate < 8_000 || sampleRate > 192_000 ||
		blockAlign*8 != channels*bits || byteRate != sampleRate*blockAlign ||
		len(samples) == 0 || len(samples)%blockAlign != 0 {
		return 0, 0, false
	}
	if encoding == 3 {
		for i := 0; i+4 <= len(samples); i += 4 {
			f := math.Float32frombits(le.Uint32(samples[i:]))
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) || f < -1 || f > 1 {
				return 0, 0, false
			}
		}
	}
	return sampleRate, len(samples) / blockAlign, true
}
