package main

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// fakeMaxFrames is tts-server's -n, and fakeFrameSamples its samples per
// frame.
const (
	fakeMaxFrames    = 256
	fakeFrameSamples = 1920
)

// addSpeech answers tts-server's /v1/generate with silence, as long as the
// real server was measured to speak (2026-11-01): prose takes about 0.7 frames
// a character, and digits and their punctuation far more, so a 256-character
// list of numbers runs into the frame cap and is cut with a normal 200.
func addSpeech(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/generate", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Input string }
		json.NewDecoder(r.Body).Decode(&body)
		dieIfAsked(w, body.Input)
		if strings.Contains(body.Input, "fake:slow") {
			time.Sleep(400 * time.Millisecond)
		}
		tenths := 0
		for _, c := range body.Input {
			if unicode.IsDigit(c) || c == ',' {
				tenths += 20
			} else {
				tenths += 7
			}
		}
		frames := min(max(tenths/10, 1), fakeMaxFrames)
		if strings.Contains(body.Input, "fake:cut") {
			frames = fakeMaxFrames
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Audio-Format", "s16le")
		w.Header().Set("X-Sample-Rate", "24000")
		w.Write(fakeVoice(frames * fakeFrameSamples))
	})
}

// fakeVoice is a quiet tone that swells and fades, so a drawn waveform has a
// shape to show.
func fakeVoice(samples int) []byte {
	out := make([]byte, samples*2)
	for i := range samples {
		swell := math.Abs(math.Sin(float64(i) / 9000))
		v := int16(swell * 8000 * math.Sin(float64(i)/8))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}
