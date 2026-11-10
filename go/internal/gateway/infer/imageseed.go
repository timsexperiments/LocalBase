package infer

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"regexp"
	"strconv"
)

// The seed an image was actually made with (DESIGN §24): sd-server writes it
// in the PNG's "parameters" text, after the prompt, so the last match is the
// real one whatever the prompt says.
var imageSeedRe = regexp.MustCompile(`\bSeed: (\d+), Size: `)

// imageSeedFromPNG reads that seed. It stops at the image data: sd-server
// writes its text before it.
func imageSeedFromPNG(b []byte) (int64, bool) {
	const signature = "\x89PNG\r\n\x1a\n"
	if !bytes.HasPrefix(b, []byte(signature)) {
		return 0, false
	}
	b = b[len(signature):]
	for len(b) >= 12 {
		size := binary.BigEndian.Uint32(b)
		kind := string(b[4:8])
		if kind == "IDAT" || kind == "IEND" || uint64(size)+12 > uint64(len(b)) {
			return 0, false
		}
		if keyword, text, ok := bytes.Cut(b[8:8+size], []byte{0}); ok && kind == "tEXt" && string(keyword) == "parameters" {
			all := imageSeedRe.FindAllSubmatch(text, -1)
			if len(all) == 0 {
				return 0, false
			}
			seed, err := strconv.ParseInt(string(all[len(all)-1][1]), 10, 64)
			return seed, err == nil
		}
		b = b[12+size:]
	}
	return 0, false
}

// annotateImageSeeds adds data[i].seed to an image response: what the image
// file says, else the requested seed + i (sd-server's batch rule; JPEG and
// WebP carry no parameters). With neither, the seed is unknown and the field
// is left out rather than guessed.
func annotateImageSeeds(obj map[string]any, requested *int64) {
	data, _ := obj["data"].([]any)
	for i, d := range data {
		item, ok := d.(map[string]any)
		if !ok {
			continue
		}
		enc, _ := item["b64_json"].(string)
		raw, err := base64.StdEncoding.DecodeString(enc)
		if seed, ok := imageSeedFromPNG(raw); err == nil && ok {
			item["seed"] = seed
		} else if requested != nil {
			item["seed"] = *requested + int64(i)
		}
	}
}

// sdSeed is the requested seed when the backend is sd-server, whose rule
// annotateImageSeeds relies on; another adapter may not honor it.
func sdSeed(adapter string, t imageTuning) *int64 {
	if adapter == "sd-server/v1" {
		return t.seed
	}
	return nil
}
