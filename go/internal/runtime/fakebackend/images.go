package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// addImages answers sd-server's image routes with a tiny PNG. A prompt
// containing "fake:slow-image" takes 1.5 s, and a client that goes away
// first is recorded in FAKE_CANCEL_LOG.
func addImages(mux *http.ServeMux) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake-image"))
	answer := func(w http.ResponseWriter, r *http.Request, prompt string) {
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
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"created": time.Now().Unix(), "data": []any{map[string]any{"b64_json": png}}})
	}
	mux.HandleFunc("POST /v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Prompt string }
		json.NewDecoder(r.Body).Decode(&body)
		answer(w, r, body.Prompt)
	})
	mux.HandleFunc("POST /v1/images/edits", func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(32 << 20)
		answer(w, r, r.FormValue("prompt"))
	})
}
