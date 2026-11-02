package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var fakeSeedRe = regexp.MustCompile(`<sd_cpp_extra_args>.*"seed":(\d+).*</sd_cpp_extra_args>`)

// fakePNG is a real small picture a browser can decode, its colors following the
// seed, with sd-server's "parameters" text, enough for the gateway to read the
// seed.
func fakePNG(prompt string, seed int64) string {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			img.Set(x, y, color.RGBA{uint8(seed*37) + uint8(x), uint8(seed*91) + uint8(y), 180, 255})
		}
	}
	var encoded bytes.Buffer
	png.Encode(&encoded, img)
	raw := encoded.Bytes()
	const afterIHDR = 8 + 25 // the signature, then the first chunk
	var b bytes.Buffer
	b.Write(raw[:afterIHDR])
	data := []byte(fmt.Sprintf("parameters\x00%s\nSteps: 4, Seed: %d, Size: 256x256, SDCPP: {\"seed\":%d}", prompt, seed, seed))
	binary.Write(&b, binary.BigEndian, uint32(len(data)))
	body := append([]byte("tEXt"), data...)
	b.Write(body)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(body))
	b.Write(raw[afterIHDR:])
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

// addImages answers sd-server's image routes: n images with seeds base + i,
// where the base is the prompt's seed or 42, as the real server does. A prompt
// containing "fake:slow-image" takes 1.5 s, and a client that goes away first
// is recorded in FAKE_CANCEL_LOG.
func addImages(mux *http.ServeMux) {
	answer := func(w http.ResponseWriter, r *http.Request, prompt string, n int) {
		if strings.Contains(prompt, "fake:slow-image") {
			select {
			case <-time.After(1500 * time.Millisecond):
			case <-r.Context().Done():
				if f := os.Getenv("FAKE_CANCEL_LOG"); f != "" {
					if fh, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
						fmt.Fprintln(fh, "cancelled")
						fh.Close()
					}
				}
				return
			}
		}
		base := int64(42)
		if m := fakeSeedRe.FindStringSubmatch(prompt); m != nil {
			base, _ = strconv.ParseInt(m[1], 10, 64)
		}
		data := []any{}
		for i := range max(n, 1) {
			data = append(data, map[string]any{"b64_json": fakePNG(prompt, base+int64(i))})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"created": time.Now().Unix(), "data": data})
	}
	mux.HandleFunc("POST /v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt string
			N      int
		}
		json.NewDecoder(r.Body).Decode(&body)
		answer(w, r, body.Prompt, body.N)
	})
	mux.HandleFunc("POST /v1/images/edits", func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(32 << 20)
		n, _ := strconv.Atoi(r.FormValue("n"))
		answer(w, r, r.FormValue("prompt"), n)
	})
}
