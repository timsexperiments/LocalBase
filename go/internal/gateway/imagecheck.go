package gateway

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"slices"
)

// Upload checks for image edits (TS image-validation.ts): structural PNG and
// JPEG validation before anything reaches the native decoder.

type imageDims struct{ width, height int }

var pngSignature = []byte{137, 80, 78, 71, 13, 10, 26, 10}

func imageMimeFromMagic(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], pngSignature):
		return "image/png"
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "image/jpeg"
	}
	return ""
}

func validPNGDepth(color, depth int) bool {
	switch color {
	case 0:
		return slices.Contains([]int{1, 2, 4, 8, 16}, depth)
	case 2, 4, 6:
		return depth == 8 || depth == 16
	case 3:
		return slices.Contains([]int{1, 2, 4, 8}, depth)
	}
	return false
}

func pngChannels(color int) int {
	switch color {
	case 0, 3:
		return 1
	case 2:
		return 3
	case 4:
		return 2
	}
	return 4
}

var adam7 = [7][4]int{{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2}}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// pngPasses returns each pass's (width, height); one pass when not interlaced.
func pngPasses(width, height, interlace int) [][2]int {
	if interlace == 0 {
		return [][2]int{{width, height}}
	}
	var out [][2]int
	for _, p := range adam7 {
		w, h := 0, 0
		if width > p[0] {
			w = ceilDiv(width-p[0], p[2])
		}
		if height > p[1] {
			h = ceilDiv(height-p[1], p[3])
		}
		if w > 0 && h > 0 {
			out = append(out, [2]int{w, h})
		}
	}
	return out
}

// validatePNG checks structure and bounds decompression to the exact
// scanline size.
func validatePNG(b []byte) *imageDims {
	if len(b) < 8 || !bytes.Equal(b[:8], pngSignature) {
		return nil
	}
	be := binary.BigEndian
	offset := 8
	var dims *imageDims
	bitDepth, colorType, interlace := 0, 0, 0
	sawIDAT, sawPalette, idatEnded, sawIEND := false, false, false, false
	paletteEntries := 0
	var compressed []byte
	for offset+12 <= len(b) {
		length := int(be.Uint32(b[offset:]))
		dataStart := offset + 8
		next := dataStart + length + 4
		if next > len(b) || next < dataStart {
			return nil
		}
		typ := string(b[offset+4 : offset+8])
		if typ != "IHDR" && typ != "PLTE" && typ != "IDAT" && typ != "IEND" && b[offset+4]&0x20 == 0 {
			return nil
		}
		if dims == nil {
			if typ != "IHDR" || offset != 8 || length != 13 {
				return nil
			}
			width, height := int(be.Uint32(b[dataStart:])), int(be.Uint32(b[dataStart+4:]))
			bitDepth, colorType = int(b[dataStart+8]), int(b[dataStart+9])
			if width == 0 || height == 0 || width > 2048 || height > 2048 || width*height > 4_194_304 ||
				!validPNGDepth(colorType, bitDepth) || b[dataStart+10] != 0 || b[dataStart+11] != 0 ||
				(b[dataStart+12] != 0 && b[dataStart+12] != 1) {
				return nil
			}
			interlace = int(b[dataStart+12])
			dims = &imageDims{width, height}
		} else if typ == "IHDR" {
			return nil
		}
		switch typ {
		case "PLTE":
			if sawIDAT || sawPalette || length == 0 || length > 768 || length%3 != 0 || colorType == 0 || colorType == 4 {
				return nil
			}
			if colorType == 3 && length/3 > 1<<bitDepth {
				return nil
			}
			sawPalette, paletteEntries = true, length/3
		case "tRNS":
			// The backend's stb_image decoder rejects these tRNS layouts.
			if sawIDAT {
				return nil
			}
			if colorType == 3 {
				if !sawPalette || length > paletteEntries {
					return nil
				}
			} else if colorType == 4 || colorType == 6 || length != pngChannels(colorType)*2 {
				return nil
			}
		}
		if typ == "IDAT" {
			if idatEnded || sawIEND || (colorType == 3 && !sawPalette) {
				return nil
			}
			sawIDAT = true
			compressed = append(compressed, b[dataStart:dataStart+length]...)
		} else {
			if sawIDAT && typ != "IEND" {
				idatEnded = true
			}
			if typ == "IEND" {
				if length != 0 || next != len(b) || sawIEND {
					return nil
				}
				sawIEND = true
				offset = next
				break
			}
		}
		offset = next
	}
	if dims == nil || !sawIDAT || !sawIEND || offset != len(b) {
		return nil
	}
	bitsPerPixel := pngChannels(colorType) * bitDepth
	expected := 0
	for _, p := range pngPasses(dims.width, dims.height, interlace) {
		expected += (ceilDiv(p[0]*bitsPerPixel, 8) + 1) * p[1]
	}
	if expected == 0 || expected >= 0x7fffffff {
		return nil
	}
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(zr, int64(expected)+1))
	if err != nil || len(raw) != expected {
		return nil
	}
	// Each scanline starts with a filter type of 0-4.
	at := 0
	for _, p := range pngPasses(dims.width, dims.height, interlace) {
		row := ceilDiv(p[0]*bitsPerPixel, 8)
		for y := 0; y < p[1]; y++ {
			if raw[at] > 4 {
				return nil
			}
			at += row + 1
		}
	}
	if at != len(raw) {
		return nil
	}
	return dims
}

// validateJPEG checks segment bounds, SOF/SOS presence, entropy markers and
// EOI.
func validateJPEG(b []byte) *imageDims {
	if len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
		return nil
	}
	be := binary.BigEndian
	offset := 2
	var dims *imageDims
	sawSOS := false
	for offset < len(b) {
		if sawSOS {
			for offset < len(b) && b[offset] != 0xff {
				offset++
			}
			if offset >= len(b) {
				return nil
			}
		}
		if b[offset] != 0xff {
			return nil
		}
		for offset < len(b) && b[offset] == 0xff {
			offset++
		}
		if offset >= len(b) {
			return nil
		}
		marker := b[offset]
		offset++
		switch {
		case marker == 0x00:
			if !sawSOS {
				return nil
			}
			continue
		case marker == 0xd9:
			if sawSOS && dims != nil && offset == len(b) {
				return dims
			}
			return nil
		case marker >= 0xd0 && marker <= 0xd7:
			if !sawSOS {
				return nil
			}
			continue
		case marker == 0xd8:
			return nil
		case marker == 0x01:
			continue
		}
		if offset+2 > len(b) {
			return nil
		}
		length := int(be.Uint16(b[offset:]))
		if length < 2 || offset+length > len(b) {
			return nil
		}
		if marker >= 0xc0 && marker <= 0xcf && marker != 0xc4 && marker != 0xc8 && marker != 0xcc {
			if (marker != 0xc0 && marker != 0xc1 && marker != 0xc2) || length < 8 || b[offset+2] != 8 || dims != nil ||
				length != 8+3*int(b[offset+7]) {
				return nil
			}
			dims = &imageDims{height: int(be.Uint16(b[offset+3:])), width: int(be.Uint16(b[offset+5:]))}
			if dims.width == 0 || dims.height == 0 {
				return nil
			}
		}
		if marker == 0xcc {
			return nil
		}
		if marker == 0xda {
			if dims == nil {
				return nil
			}
			n := 0
			if offset+2 < len(b) {
				n = int(b[offset+2])
			}
			if n < 1 || length != 6+2*n {
				return nil
			}
			sawSOS = true
		}
		offset += length
	}
	return nil
}
