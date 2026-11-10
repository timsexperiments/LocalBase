package gateway

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/browser"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
	"github.com/timsexperiments/LocalBase/go/internal/video"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

// Config is everything a gateway is built from: New takes it, and the Server keeps it.
type Config struct {
	// Placement is the CPU/GPU placement source (DESIGN §20; nil: no API).
	Placement *runtime.HostPlacement
	Store     *store.Store
	Catalog   *catalog.Catalog
	Auth      *auth.Resolver
	LLM       *runtime.LLM
	Log       *slog.Logger
	NoAuth    bool
	MemoryGB  int
	Root      *root.Root
	Memory    *memory.Controller
	Browser   *auth.Browser
	// Library joins the catalog, imports and backend selections (nil in
	// unit tests that need neither).
	Library *library.Library
	// STT, TTS and Image supervise the media modalities (nil: route disabled).
	STT   *runtime.Service
	TTS   *runtime.Service
	Image *runtime.Service
	// Video runs video jobs on VideoJobs (nil: no video runtime).
	Video     *runtime.Service
	VideoJobs *video.Manager
	// Voices stores custom voice profiles (nil: the routes are disabled).
	Voices *voices.Store
	// OnReady runs whenever readiness reports ready (a managed gateway
	// acknowledges its startup settings, TS acknowledgeStaticConfiguration).
	OnReady func()
	// Otel exports traces (nil: disabled).
	Otel *otlp.Runtime
	// Limits bounds what a request can cost (zero values take the defaults).
	Limits httpx.Limits
	// Usage keeps hourly totals (DESIGN §32; nil: not recorded, and the route is disabled).
	Usage *usage.Recorder
	// Updates is the optional daily check for a newer version (DESIGN §33; nil: the route is disabled).
	Updates *upgrade.Checker
	// UI handles /app and / when set.
	UI http.Handler
	// LoginPages renders local sign-in's pages (DESIGN §28); nil in tests without a UI.
	LoginPages browser.LoginPages
	// RunMemoryLimit and RunsMemoryLimit are what a detached chat run, and all retained runs
	// together, may hold in memory; 0 is the default.
	RunMemoryLimit, RunsMemoryLimit int64
	// RunMaxDuration is how long a detached run may take before it is interrupted; 0 is the default
	// (15 minutes). No flag or setting sets it: tests do.
	RunMaxDuration time.Duration
	// Overrides applies what only this process has (the environment, serve's flags) to a saved
	// configuration, before it is published to the runtimes.
	Overrides func(*store.Config)
	// LogOutages reports logs that can't rotate or be written, for doctor and status.
	LogOutages func() []obs.LogOutage
}

// runtimes is the runtime set the gateway was given, for the handlers that take one.
func (cfg Config) runtimes() runtime.Set {
	return runtime.Set{LLM: cfg.LLM, STT: cfg.STT, TTS: cfg.TTS, Image: cfg.Image, Video: cfg.Video}
}
