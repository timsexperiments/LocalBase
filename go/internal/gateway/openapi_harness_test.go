package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
	"github.com/timsexperiments/LocalBase/go/internal/video"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

const (
	openAPISTTModel   = "whisper-base-q8_0"
	openAPIImageModel = "dreamshaper-8-lcm"
	openAPIWait       = 20 * time.Second
	openAPIDevice     = "test-device"
)

// testOtel, when set, is the next gateway's exporter.
var testOtel *otlp.Runtime

// openAPIHasMedia: the fake media runtimes use the Metal backend entries, as the other
// media gateways here do, so generating is compared on macOS. Every platform has the
// services themselves, so the routes answer the same way everywhere.
var openAPIHasMedia = goruntime.GOOS == "darwin"

// openAPIGateway is the whole gateway, every modality wired over the fake backend, with
// authentication on from the start: every route answers as it would in production, and
// nothing changes under a running server.
// openAPIGateway is a whole gateway on a scratch root, as the OpenAPI tests use it; configure, if
// given, sets what a test needs on the server before it starts.
func openAPIGateway(t *testing.T, configure ...func(*Server)) *httptest.Server {
	t.Helper()
	r, err := root.Init(filepath.Join(scratchDir(t), "root"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		t.Fatal(err)
	}
	reg, err := backends.Load("")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	t.Cleanup(func() { noWriterLeft(t, st) })
	// The import checks use a local file, which is opt-in over the API.
	if err := st.SetSetting(context.Background(), store.LocalPathImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	llmDir := r.ModelsDir("llm")
	os.MkdirAll(llmDir, 0o700)
	for _, id := range []string{chatModel, embeddingModel, decisionModel} {
		installSparse(t, llmDir, cat.ByID(id))
	}
	cfg := &store.Config{Host: "127.0.0.1", Port: freePort(t), CtxSize: 8192, CtxMode: "auto", Parallel: "1",
		Active:   map[string]string{"llm": chatModel},
		Selected: map[string][]string{"llm": {chatModel, embeddingModel, decisionModel}}}
	cfg.Root, cfg.GatewayHost, cfg.SttHost = r.Dir, "127.0.0.1", "127.0.0.1"
	mem := memory.NewController(host{}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testLog != nil {
		log = slog.New(slog.NewJSONHandler(testLog, nil))
	}
	s := &Server{Store: st, Catalog: cat, Auth: &auth.Resolver{Store: st, Mode: "either"}, Log: log, MemoryGB: 64,
		Root: r, Memory: mem, Voices: voices.New(st, r.Dir)}

	service := func(modality, modelID, backendID, binary string) *runtime.Service {
		spec := cat.ByID(modelID)
		if spec == nil {
			t.Fatalf("no catalog entry %s", modelID)
		}
		dir := r.ModelsDir(modality)
		os.MkdirAll(dir, 0o700)
		installSparse(t, dir, spec)
		cfg.Active[modality], cfg.Selected[modality] = modelID, []string{modelID}
		m := models.FromSpec(spec)
		gate := func(_ context.Context, id string, _ bool) (*runtime.Target, error) {
			if id != modelID {
				return nil, runtime.ErrModelNotFound
			}
			return &runtime.Target{ModelID: id, Model: &m, Variant: &m.Variants[0], Backend: reg.Get(backendID), Dir: dir}, nil
		}
		svc := runtime.NewService(runtime.ServiceDeps{Modality: modality, Kind: modality, Catalog: cat, Store: st, Memory: mem, Log: log,
			Config: func() *store.Config { return cfg }, Gate: gate, ModelsDir: dir, RootDir: r.Dir, Host: "127.0.0.1",
			Port: freePort(t), Binaries: map[string]string{binary: fakeBinary},
			Verify: func(string, catalog.Artifact, string) error { return nil }})
		t.Cleanup(svc.Close)
		return svc
	}
	s.TTS = service("tts", speechModel, "tts-server", "tts-server")
	s.STT = service("stt", openAPISTTModel, "whisper.cpp-metal", "whisper-server")
	s.Image = service("image", openAPIImageModel, "stable-diffusion.cpp-metal", "sd-server")
	s.Video = service("video", videoModel, "stable-diffusion.cpp-metal", "sd-server")
	client, err := video.NewClient(s.Video.BaseURL() + "/")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := video.NewManager(video.Options{Backend: client, TempDir: filepath.Join(r.Dir, "tmp"), PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { jobs.Shutdown() })
	s.VideoJobs = jobs

	if err := st.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	s.LLM = runtime.NewLLM(runtime.LLMDeps{Root: r.Dir, ModelsDir: llmDir, Store: st, Catalog: cat, Memory: mem,
		Log: log, MemoryGB: 64, Binary: fakeBinary, QueueCapacity: 4, QueueTimeout: 5 * time.Second}, cfg)
	t.Cleanup(s.LLM.Close)
	// Placement, as serve wires it: the chat model's file has a tensor table to plan from.
	primary := cat.ByID(chatModel).Primary()
	placementtest.WriteMoEGGUF(t, filepath.Join(llmDir, primary.Filename), primary.ExpectedSizeBytes)
	s.Placement = &runtime.HostPlacement{Store: st, DeviceID: openAPIDevice, Memory: mem, Log: log, Platform: placement.Unified}
	s.LLM.SetPlacementSource(s.Placement)
	lib, err := library.Open(r, st, cat, backends.ProbeAccelerators(backends.ProbeOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	s.Library = lib
	s.Otel = testOtel
	s.Updates = upgrade.NewChecker(st, upgrade.Source{API: "http://127.0.0.1:1/never"}, "0.2.0", true, log)
	s.Usage, err = usage.New(st, log)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range configure {
		c(s)
	}
	ts := httptest.NewServer(New(s))
	t.Cleanup(ts.Close)
	return ts
}

// minimalWAV is 0.1 s of silence: 24 kHz, mono, 16-bit.
func minimalWAV() []byte { return pcm16WAV(24000, make([]byte, 4800)) }

// voiceClipWAV is a reference a voice accepts: 4 s of noise, 16 kHz, mono, 16-bit
// (the generator of voices_test.go's build).
func voiceClipWAV() []byte {
	data := make([]byte, 2*4*16000)
	x := uint32(1)
	for i := range data {
		x = x*1664525 + 1013904223
		data[i] = byte(x >> 24)
	}
	return pcm16WAV(16000, data)
}

func pcm16WAV(rate uint32, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, rate)
	binary.Write(&b, binary.LittleEndian, 2*rate)
	binary.Write(&b, binary.LittleEndian, uint16(2))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}
