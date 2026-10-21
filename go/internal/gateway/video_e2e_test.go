package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/video"
)

const videoModel = "wan2.1-t2v-1.3b-q8_0"

// fakeFFmpeg converts the fake backend's AVI: it reports the frame count
// the AVI carries and writes a small MP4.
const fakeFFmpeg = `#!/bin/bash
out="${!#}"; in=""; prev=""
for a in "$@"; do [ "$prev" = "-i" ] && in="$a"; prev="$a"; done
frames=$(LC_ALL=C grep -ao 'frames=[0-9]*' "$in" | head -1 | cut -d= -f2)
printf '\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isomiso2avc1mp41fake-mp4-payload' > "$out"
printf 'frame=%s\nprogress=end\n' "$frames"
`

type videoGateway struct {
	ts     *httptest.Server
	svc    *runtime.Service
	jobs   *video.Manager
	bodies string // the backend's submitted bodies, one JSON per line
}

func newVideoGateway(t *testing.T) *videoGateway {
	t.Helper()
	if _, _, _, ok := runtime.VideoTarget(memory.Topology{Unified: true}); !ok {
		t.Skip("video runs on macOS arm64 here")
	}
	dir := t.TempDir()
	r, err := root.Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := catalog.LoadWithFormer()
	reg, _ := backends.Load("")
	spec := cat.ByID(videoModel)
	modelsDir := r.ModelsDir("video")
	os.MkdirAll(modelsDir, 0o700)
	installSparse(t, modelsDir, spec)
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &store.Config{Active: map[string]string{"video": videoModel}, Selected: map[string][]string{"video": {videoModel}}}
	mem := memory.NewController(host{}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	m := models.FromSpec(spec)
	sd := reg.Get("stable-diffusion.cpp-metal")
	gate := func(id string) (*runtime.Target, error) {
		if id != videoModel {
			return nil, runtime.ErrModelNotFound
		}
		return &runtime.Target{ModelID: id, Model: &m, Variant: &m.Variants[0], Backend: sd, Dir: modelsDir}, nil
	}
	bodies := filepath.Join(dir, "bodies.jsonl")
	t.Setenv("FAKE_VIDEO_LOG", bodies)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := runtime.NewService(runtime.ServiceDeps{Modality: "video", Kind: "video", Catalog: cat, Store: st, Memory: mem, Log: log,
		Config: func() *store.Config { return cfg }, Gate: gate, ModelsDir: modelsDir, RootDir: r.Dir, Host: "127.0.0.1",
		Port: freePort(t), Binaries: map[string]string{"sd-server": fakeBinary},
		Verify: func(string, catalog.Artifact, string) error { return nil }})
	t.Cleanup(svc.Close)
	ffmpeg := filepath.Join(dir, "ffmpeg")
	os.WriteFile(ffmpeg, []byte(fakeFFmpeg), 0o700)
	client, err := video.NewClient(svc.BaseURL() + "/")
	if err != nil {
		t.Fatal(err)
	}
	conv := &video.Converter{Ensure: func(ctx context.Context) (string, error) { return ffmpeg, nil }}
	jobs, err := video.NewManager(video.Options{Backend: client, TempDir: filepath.Join(r.Dir, "tmp"), Prepare: conv.Prepare,
		PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobs.Shutdown() })
	srv := New(&Server{Store: st, Catalog: cat, Auth: &auth.Resolver{Store: st, Mode: "either"}, Log: log,
		NoAuth: true, MemoryGB: 64, Root: r, Memory: mem, Video: svc, VideoJobs: jobs})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &videoGateway{ts: ts, svc: svc, jobs: jobs, bodies: bodies}
}

func videoRequest(prompt string) string {
	b, _ := json.Marshal(map[string]any{"model": videoModel, "prompt": prompt, "width": 320, "height": 320,
		"frames": 33, "fps": 16, "input": map[string]any{"kind": "text"}})
	return string(b)
}

func (g *videoGateway) create(t *testing.T, prompt string) map[string]any {
	t.Helper()
	res := post(t, g.ts, "/v1/videos", videoRequest(prompt))
	if res.status != 202 {
		t.Fatalf("create: %d %s", res.status, res.body)
	}
	var job map[string]any
	json.Unmarshal([]byte(res.body), &job)
	if job["status"] != "queued" || job["object"] != "localbase.video.job" {
		t.Fatalf("created %v", job)
	}
	return job
}

func (g *videoGateway) get(t *testing.T, path string) (int, http.Header, []byte) {
	t.Helper()
	resp, err := http.Get(g.ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

// waitStatus polls a job until it leaves queued and in_progress.
func (g *videoGateway) waitStatus(t *testing.T, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		status, _, body := g.get(t, "/v1/videos/"+id)
		var job map[string]any
		json.Unmarshal(body, &job)
		if status != 200 {
			t.Fatalf("status %d %s", status, body)
		}
		if job["status"] != "queued" && job["status"] != "in_progress" {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still %v", job["status"])
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitRunningVideo waits until a job is generating on the backend.
func (g *videoGateway) waitGenerating(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, _, body := g.get(t, "/v1/videos/"+id)
		if strings.Contains(string(body), `"in_progress"`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never generated: %s", body)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestVideoJobLifecycle runs video jobs end to end on the fake sd-server
// and a fake converter: the job completes with the converted MP4, the
// backend receives the qualified native input, a second job is refused
// while one runs, a cancelled job stops its runtime, a failed backend job
// and a memory-pressure failure report their codes, and finished jobs can
// be deleted.
func TestVideoJobLifecycle(t *testing.T) {
	g := newVideoGateway(t)

	job := g.create(t, "a red fox in the snow")
	id := job["id"].(string)
	done := g.waitStatus(t, id)
	if done["status"] != "completed" || done["content_type"] != "video/mp4" || done["frames"] != float64(33) ||
		done["fps"] != float64(16) || done["bytes"] == float64(0) {
		t.Fatalf("completed %v", done)
	}
	status, h, body := g.get(t, "/v1/videos/"+id+"/content")
	if status != 200 || h.Get("Content-Type") != "video/mp4" || h.Get("Cache-Control") != "private, no-store" ||
		h.Get("X-Content-Type-Options") != "nosniff" || string(body[4:8]) != "ftyp" || float64(len(body)) != done["bytes"] {
		t.Fatalf("content %d %v %q", status, h, body)
	}
	raw, _ := os.ReadFile(g.bodies)
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Scan()
	var sent map[string]any
	json.Unmarshal(sc.Bytes(), &sent)
	sp, _ := sent["sample_params"].(map[string]any)
	if sent["prompt"] != "a red fox in the snow" || sent["width"] != float64(320) || sent["video_frames"] != float64(33) ||
		sent["fps"] != float64(16) || sent["output_format"] != "avi" || sp["sample_method"] != "euler" {
		t.Fatalf("backend body %v", sent)
	}

	// One job at a time.
	slow := g.create(t, "fake:slow")
	g.waitGenerating(t, slow["id"].(string))
	if res := post(t, g.ts, "/v1/videos", videoRequest("another")); res.status != 429 || res.code() != "video_job_busy" ||
		res.header.Get("Retry-After") != "1" {
		t.Fatalf("busy: %d %v %s", res.status, res.header, res.body)
	}
	// Cancel stops the runtime under the job.
	res := post(t, g.ts, "/v1/videos/"+slow["id"].(string)+"/cancel", "")
	if res.status != 200 || !strings.Contains(res.body, `"cancellation_reason":"cancelled"`) {
		t.Fatalf("cancel: %d %s", res.status, res.body)
	}
	if g.svc.Running() != "" {
		t.Fatalf("runtime still running %q after cancel", g.svc.Running())
	}

	failed := g.create(t, "fake:fail")
	if f := g.waitStatus(t, failed["id"].(string)); f["status"] != "failed" ||
		f["error"].(map[string]any)["code"] != "video_generation_failed" {
		t.Fatalf("failed %v", f)
	}

	// Memory pressure fails the active job with insufficient_memory.
	pressured := g.create(t, "fake:slow")
	g.waitGenerating(t, pressured["id"].(string))
	g.jobs.FailActiveForMemoryPressure()
	p := g.waitStatus(t, pressured["id"].(string))
	e, _ := p["error"].(map[string]any)
	if p["status"] != "failed" || e["code"] != "insufficient_memory" ||
		!strings.HasPrefix(e["message"].(string), "The host is under memory pressure") {
		t.Fatalf("pressure %v", p)
	}

	// Delete removes a finished job; its content goes with it.
	req, _ := http.NewRequest(http.MethodDelete, g.ts.URL+"/v1/videos/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("delete: %v %v", err, resp)
	}
	resp.Body.Close()
	if status, _, body := g.get(t, "/v1/videos/"+id+"/content"); status != 404 || !strings.Contains(string(body), "video_job_not_found") {
		t.Fatalf("after delete %d %s", status, body)
	}
}

// TestVideoJobRejections: requests that never start a job.
func TestVideoJobRejections(t *testing.T) {
	g := newVideoGateway(t)
	mismatch := strings.Replace(videoRequest("p"), `"width":320`, `"width":336`, 1)
	if res := post(t, g.ts, "/v1/videos", mismatch); res.status != 400 || !strings.Contains(res.body, "accepts exactly 320x320") {
		t.Fatalf("mismatch: %d %s", res.status, res.body)
	}
	unknown := strings.Replace(videoRequest("p"), videoModel, "wan9-nope", 1)
	if res := post(t, g.ts, "/v1/videos", unknown); res.status != 404 || res.code() != "model_not_found" {
		t.Fatalf("unknown model: %d %s", res.status, res.body)
	}
	if status, _, body := g.get(t, "/v1/videos/6f1c2c3e-8a7b-4c1d-9e2f-0123456789ab"); status != 404 ||
		!strings.Contains(string(body), "video_job_not_found") {
		t.Fatalf("missing job %d %s", status, body)
	}
	if status, _, _ := g.get(t, "/v1/videos/not-a-job"); status == 200 {
		t.Fatal("a malformed job id matched a route")
	}
	if res := post(t, g.ts, "/v1/videos/6f1c2c3e-8a7b-4c1d-9e2f-0123456789ab", ""); res.status != 405 {
		t.Fatalf("method: %d %s", res.status, res.body)
	}
}
