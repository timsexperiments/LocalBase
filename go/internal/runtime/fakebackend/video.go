package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The stable-diffusion.cpp video API (sd-server vid_gen): GET / for health,
// POST /sdcpp/v1/vid_gen and GET /sdcpp/v1/jobs/{id}. A job completes with a
// small AVI after two polls. The prompt selects a behavior: "fake:fail" fails
// the job, "fake:cancel" ends it cancelled, "fake:slow" keeps it generating,
// "fake:bad-media" completes with bytes that are not an AVI.
// FAKE_VIDEO_LOG=<path> appends each submitted body (one JSON line).

type videoJob struct {
	id, prompt string
	frames     int
	fps        int
	polls      int
}

func addVideo(mux *http.ServeMux) {
	var mu sync.Mutex
	jobs := map[string]*videoJob{}
	n := 0
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /sdcpp/v1/vid_gen", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if p := os.Getenv("FAKE_VIDEO_LOG"); p != "" {
			if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
				b, _ := json.Marshal(body)
				f.Write(append(b, '\n'))
				f.Close()
			}
		}
		frames, _ := body["video_frames"].(float64)
		fps, _ := body["fps"].(float64)
		prompt, _ := body["prompt"].(string)
		mu.Lock()
		n++
		j := &videoJob{id: "job_" + strconv.Itoa(n), prompt: prompt, frames: int(frames), fps: int(fps)}
		jobs[j.id] = j
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"id": j.id, "kind": "vid_gen", "status": "queued", "created": time.Now().Unix()})
	})
	mux.HandleFunc("GET /sdcpp/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		j := jobs[r.PathValue("id")]
		if j != nil {
			j.polls++
		}
		mu.Unlock()
		if j == nil {
			http.NotFound(w, r)
			return
		}
		env := map[string]any{"id": j.id, "kind": "vid_gen", "created": time.Now().Unix()}
		switch {
		case strings.HasPrefix(j.prompt, "fake:slow") || j.polls < 2:
			env["status"] = "generating"
		case strings.HasPrefix(j.prompt, "fake:fail"):
			env["status"] = "failed"
		case strings.HasPrefix(j.prompt, "fake:cancel"):
			env["status"] = "cancelled"
		default:
			media := fakeAVI(j.frames)
			if strings.HasPrefix(j.prompt, "fake:bad-media") {
				media = []byte("not a video container")
			}
			env["status"] = "completed"
			env["result"] = map[string]any{"output_format": "avi", "mime_type": "video/x-msvideo", "fps": j.fps,
				"frame_count": j.frames, "b64_json": base64.StdEncoding.EncodeToString(media)}
		}
		json.NewEncoder(w).Encode(env)
	})
}

// fakeAVI is a RIFF/AVI container header with a frame count marker.
func fakeAVI(frames int) []byte {
	b := make([]byte, 64)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "AVI ")
	copy(b[12:], fmt.Sprintf("frames=%d", frames))
	return b
}
