package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// addSTT answers whisper-server's /inference with a fixed transcript, in the format
// asked for. A translation says so in its text.
func addSTT(mux *http.ServeMux) {
	mux.HandleFunc("POST /inference", func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(64 << 20)
		lines := []string{"The harbor was quiet this morning.", "A few gulls circled above the boats.", "Nobody seemed in a hurry."}
		if r.FormValue("translate") == "true" {
			lines[0] = "Translated: the harbor was quiet."
		}
		text := ""
		var segments []map[string]any
		for i, line := range lines {
			text += " " + line
			segments = append(segments, map[string]any{"id": i, "start": float64(i) * 1.2, "end": float64(i+1) * 1.2, "text": " " + line})
		}
		switch r.FormValue("response_format") {
		case "verbose_json":
			json.NewEncoder(w).Encode(map[string]any{"task": "transcribe", "language": "en", "duration": 3.6, "text": text, "segments": segments})
		case "text":
			fmt.Fprint(w, text)
		default:
			json.NewEncoder(w).Encode(map[string]any{"text": text})
		}
	})
}
