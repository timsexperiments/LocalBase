package main

import (
	"encoding/json"
	"net/http"
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
		tenths := 0
		for _, c := range body.Input {
			if unicode.IsDigit(c) || c == ',' {
				tenths += 20
			} else {
				tenths += 7
			}
		}
		frames := min(max(tenths/10, 1), fakeMaxFrames)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Audio-Format", "s16le")
		w.Header().Set("X-Sample-Rate", "24000")
		w.Write(make([]byte, frames*fakeFrameSamples*2))
	})
}
