package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/audit"
	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/conf"
	"github.com/timsexperiments/LocalBase/go/internal/gateway"
	"github.com/timsexperiments/LocalBase/go/internal/install"
	"github.com/timsexperiments/LocalBase/go/internal/lease"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/netguard"
	"github.com/timsexperiments/LocalBase/go/internal/netlimit"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/safefile"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
	"github.com/timsexperiments/LocalBase/go/internal/version"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
	"github.com/timsexperiments/LocalBase/go/internal/web"
)

// How long a stop gets. Requests drain until stopDrainFor, every backend is killed at stopKillAt
// if any is left, and the process leaves at stopExitAt whatever is still going: all inside the
// 15 s launchd, systemd and `upgrade` allow.
const (
	stopDrainFor = 9 * time.Second
	stopKillAt   = 12 * time.Second
	stopExitAt   = 13500 * time.Millisecond
)

// Options is what a gateway run was asked for, already checked: serve fills it from its flags.
// A zero value means "as configured" wherever one is configured.
type Options struct {
	// AllowRoot serves even as root (--allow-root).
	AllowRoot bool
	// LogLevel is the least severe event logged.
	LogLevel slog.Level
	// Host and Port are where the gateway listens; "" and 0 are the configured ones.
	Host string
	Port int
	// LLMHost is llama-server's host ("" is the configured one), and LLMPort its port (0 is the
	// default). It is already in the PortBase block unless a port was given.
	LLMHost string
	LLMPort int
	// PortBase is the first of the five backend ports in a row (runtime.PortOrder), 0 for none.
	PortBase int
	// Media is what was asked of each media runtime (stt, tts, image, video): one left out is on,
	// on its usual host and port.
	Media map[string]MediaOptions
	// CtxSize overrides the LLM context limit, 0 for none.
	CtxSize int
	// QueueCapacity and QueueTimeout limit each inference queue, 0 for the defaults.
	QueueCapacity int
	QueueTimeout  time.Duration
	// RunMemoryLimit and RunsMemoryLimit bound detached chat runs, in bytes; 0 for the defaults.
	RunMemoryLimit  int64
	RunsMemoryLimit int64
	// MaxConnections and MaxConnectionsPerIP cap open connections; nil is the configured value,
	// else the default.
	MaxConnections      *int
	MaxConnectionsPerIP *int
	// LLMModelFile overrides the LLM's model filename.
	LLMModelFile string
	// AuthMode is the API key header mode: bearer, x-api-key or either.
	AuthMode string
	// NoAuth turns API key authentication off (local development only, on loopback).
	NoAuth bool
	// InstallMissing downloads selected models that are missing (or the configuration says to).
	InstallMissing bool
	// BypassMemoryCheck starts models despite memory warnings.
	BypassMemoryCheck bool
	// EventsToStderr prints the events as well in a foreground run (never as a service).
	EventsToStderr bool

	// stopStuck (tests) runs when the stop begins; nil is labStopStuck, which is nothing outside a
	// localbase_lab build.
	stopStuck func()
	// afterSignals (tests) runs once the run listens for stop signals, so a test can send one then.
	afterSignals func()
	// effectiveUID (tests) is the user the run is as; nil is os.Geteuid.
	effectiveUID func() int
}

// MediaOptions is what was asked of one media runtime. Port is already checked; 0 is the
// PortBase block's, else the configured or default one.
type MediaOptions struct {
	Off  bool
	Host string
	Port int
}

// Deps is the opened root: what Opener returns.
type Deps struct {
	Root    *root.Root
	Store   *store.Store
	Config  *store.Config
	Catalog *catalog.Catalog
	Host    memory.HostProvider
	// Audit is where the run's audit records go (the first-run sign-in link).
	Audit *audit.Log
}

// Opener opens the root. It is called once, after the root check and signal setup; the returned
// func closes what it opened.
type Opener func() (Deps, func(), error)

// Reporter is told how the run goes, for the command line's output. Run calls its methods from its
// own goroutine, one at a time, and waits for each to return.
type Reporter interface {
	// Streaming is called once, before anything starts: the run's output comes as it happens.
	Streaming()
	// Started is called once the gateway listens, with its address, which kinds are enabled, and
	// the first-run sign-in link ("" for none).
	Started(baseURL string, enabled map[string]bool, loginLink string)
	// Stopped is called after a stop that went as asked, and not when the listener failed.
	Stopped()
}

// BadInput is a refusal of what the run was asked for, in its own words. The command line answers it
// as bad input (exit 2).
type BadInput struct{ Message string }

func (e *BadInput) Error() string { return e.Message }

// rootRefusal is why the run won't go ahead as root.
const rootRefusal = "Refusing to serve as root: a gateway runs the backends it downloads and reads model files from anywhere, and all of that would run as root. Run it as an ordinary user, or pass --allow-root in a container that holds nothing else."

// badNoAuth answers a --no-auth refusal as bad input, in its own words.
func badNoAuth(err error) error {
	var r *netguard.NoAuthRefusal
	if errors.As(err, &r) {
		return &BadInput{Message: r.Message}
	}
	return err
}

// Run is the gateway, from opening the root until it has stopped.
func Run(opts Options, open Opener, rep Reporter) error {
	effectiveUID := opts.effectiveUID
	if effectiveUID == nil {
		effectiveUID = os.Geteuid
	}
	if effectiveUID() == 0 && !opts.AllowRoot {
		return &BadInput{Message: rootRefusal}
	}
	rep.Streaming()

	// From here on a stop signal waits its turn: it is read once startup is done, and everything
	// started so far is closed by its defer. Before, a SIGTERM during startup ended the process
	// with none of them.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if opts.afterSignals != nil {
		opts.afterSignals()
	}

	deps, closeDeps, err := open()
	if err != nil {
		return err
	}
	defer closeDeps()
	r, st, cfg, cat := deps.Root, deps.Store, deps.Config, deps.Catalog
	if err := conf.ApplyEnv(cfg); err != nil {
		var de *conf.DocumentError
		if errors.As(err, &de) {
			return &BadInput{Message: de.Message}
		}
		return err
	}

	// Before anything writes to them: a planted link must not redirect a log (TS).
	if problems := obs.CheckRootFiles(r.Dir); len(problems) > 0 {
		// A service manager retries this: the event is once per cause, and stderr
		// (the returned error) says it every time.
		if obs.RecordStartRefusal(r.Dir, problems[0], time.Now()) {
			obs.LogStartRefusal(r.Dir, r.LogsDir(), problems[0])
		}
		return problems[0]
	}
	obs.ClearStartRefusal(r.Dir)
	if os.Getenv("LOCALBASE_SERVICE_TOKEN") != "" {
		if err := obs.BoundServiceStderr(r.Dir); err != nil { // #357: launchd never rotates the capture
			var unsafe *safefile.UnsafeFileError
			if errors.As(err, &unsafe) {
				return err
			}
		}
	}
	sink, err := obs.OpenSink(r.LogsDir())
	if err != nil {
		return err
	}
	defer sink.Close()
	if os.Getenv("LOCALBASE_SERVICE_TOKEN") == "" && opts.EventsToStderr {
		sink.Mirror = os.Stderr // foreground serve also prints events
	}
	handler := obs.NewHandler(sink, opts.LogLevel)
	log := slog.New(handler)
	st.Log = log
	if n := root.TightenPermissions(r.Dir, func(path string, err error) {
		log.Warn("Could not make a file private.", "event", "root.permissions-failed", "path", path, "error", err.Error())
	}); n > 0 {
		log.Info("Made existing files private.", "event", "root.permissions-tightened", "count", n)
	}
	for _, d := range cat.Diagnostics {
		log.Warn("Skipped a catalog file: "+d.Message, "event", "catalog.entry-skipped", "path", d.Path)
	}

	listenHost, listenPort := cfg.GatewayHost, cfg.GatewayPort
	if opts.Host != "" {
		listenHost = opts.Host
	}
	if opts.Port != 0 {
		listenPort = opts.Port
	}
	if opts.LLMHost != "" {
		cfg.Host = opts.LLMHost
	}
	if opts.NoAuth {
		if err := badNoAuth(netguard.NoAuthHost(listenHost)); err != nil {
			return err
		}
	}
	// Hold the operation lease (or claim the one a service start handed off)
	// until this process owns the gateway lock.
	initLease, err := lease.ServeInitialization(r.Dir, os.Getenv("LOCALBASE_SERVICE_TOKEN"))
	if err != nil {
		return err
	}
	release, err := r.LockAs(listenHost, listenPort, os.Getenv("LOCALBASE_SERVICE_ID"), os.Getenv("LOCALBASE_SERVICE_TOKEN"))
	initLease.Release()
	if err != nil {
		return err
	}
	// With the root's lock held, whatever an earlier gateway left running is not
	// serving anyone: it goes before this one starts backends on the same ports.
	runtime.SweepBackends(r.Dir, log)
	defer release()
	tel, err := otlp.Start(cfg.OtelEndpoint, cfg.OtelHeaders, cfg.OtelSampleRatio, handler, log)
	if err != nil {
		return err
	}
	defer tel.Shutdown() // after gateway.stopped, before the log closes

	// The GPU limit from Metal, once the managed llama-server exists. It is not
	// waited for: admission uses the heuristic until it answers. A first run has no
	// binary yet, so the next load tries again.
	var gpuLimitLogged sync.Once
	probeGPU := func() {
		bin, err := runtime.ResolveBinary(r.RuntimesDir(), "llama-server")
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if v, ok := memory.ProbeGPUWorkingSet(ctx, deps.Host, bin); ok {
			gpuLimitLogged.Do(func() {
				log.Info("Read the GPU memory limit from Metal.", "event", "memory.gpu-limit", "gpu_limit_bytes", v, "source", "metal")
			})
		}
	}
	go probeGPU()
	memoryGB := int(float64(deps.Host.Topology().SystemCapacity)/float64(memory.GiB) + 0.5)
	mem := memory.NewController(deps.Host, memory.Reserve{Percent: cfg.SystemReserve.Percent, MinimumGB: cfg.SystemReserve.MinimumGB},
		opts.BypassMemoryCheck)
	lib, err := library.Open(r, st, cat, backends.ProbeAccelerators(backends.ProbeOptions{}))
	if err != nil {
		return err
	}
	lib.Log = log
	for _, d := range lib.Registry.Skipped() {
		log.Warn("Skipped a custom backend definition: "+d, "event", "backend.definition-skipped")
	}
	installMissing := opts.InstallMissing || cfg.InstallMissingModels
	// A model refused for memory may stop the idle runtimes of other modalities.
	recovery := &runtime.IdleRecovery{Memory: mem, Log: log}
	var llm *runtime.LLM
	// CPU/GPU placement per model on this device (DESIGN §20).
	placementSource := &runtime.HostPlacement{Store: st, DeviceID: lib.DeviceID, Memory: mem, Log: log}
	// A switch to another configured model makes it the active one, as in TS.
	activate := func(kind, modelID string) {
		if err := conf.RecordActive(r.Dir, st, llm, kind, modelID); err != nil {
			log.Warn("Could not record the active "+kind+" model.", "event", "config.save-failed", "error", err.Error())
		}
	}
	llm = runtime.NewLLM(runtime.LLMDeps{Root: r.Dir, ModelsDir: r.ModelsDir("llm"), RuntimesDir: r.RuntimesDir(),
		Store: st, Catalog: cat, Memory: mem, Log: log, MemoryGB: float64(memoryGB), CtxOverride: opts.CtxSize,
		PortOverride: opts.LLMPort, QueueCapacity: opts.QueueCapacity, QueueTimeout: opts.QueueTimeout,
		Binary: os.Getenv("LOCALBASE_LLAMA_SERVER"), ModelFileOverride: opts.LLMModelFile,
		BeforeSwitch: probeGPU, Install: installer(deps, installMissing, log), Gate: lib.Gate(), SelectionKey: lib.SelectionKey, Activate: activate, Otel: tel, Recovery: recovery,
		Placement: placementSource, GPUPool: &memory.GPUPool{Reserve: placement.ReserveBytes}}, cfg)
	recovery.Add(llm)
	defer llm.Close()
	// The media modalities: one supervised backend each, through the selection.
	lib.InstallMissing = installMissing
	lib.HFToken = func() string { return llm.Config().HFToken }
	hostPort := func(name, host string, port int) (string, int) {
		m := opts.Media[name]
		if m.Host != "" {
			host = m.Host
		}
		if m.Port != 0 {
			return host, m.Port
		}
		return host, runtime.PortInBlock(opts.PortBase, name, port)
	}
	binaries := map[string]string{"whisper-server": os.Getenv("LOCALBASE_WHISPER_SERVER"), "tts-server": os.Getenv("LOCALBASE_TTS_SERVER"),
		"crispasr": os.Getenv("LOCALBASE_CRISPASR"), "sd-server": os.Getenv("LOCALBASE_SD_SERVER")}
	service := func(modality, task, host string, port int) *runtime.Service {
		if opts.Media[modality].Off {
			return nil
		}
		h, p := hostPort(modality, host, port)
		sd := runtime.ServiceDeps{Modality: modality, Kind: modality, Catalog: cat, Store: st, Memory: mem, Log: log,
			Config: llm.Config, Gate: lib.GateFor(task), SelectionKey: lib.SelectionKey, RuntimesDir: r.RuntimesDir(),
			ModelsDir: r.ModelsDir(modality), RootDir: r.Dir, CacheDir: filepath.Join(r.Dir, "cache"), Host: h, Port: p,
			Binaries: binaries, Install: installer(deps, installMissing, log), Activate: activate,
			QueueCapacity: opts.QueueCapacity, QueueTimeout: opts.QueueTimeout, Otel: tel, Recovery: recovery}
		if modality == "tts" {
			// A changed voice set or marking setting restarts the speech server.
			sd.ExtraKey = func() string {
				return voices.Stamp(context.Background(), st) + "|marking=" + strconv.FormatBool(runtime.SpeechMarking(context.Background(), st))
			}
		}
		return runtime.NewService(sd)
	}
	stt := service("stt", "transcription", cfg.SttHost, cfg.SttPort)
	tts := service("tts", "speech", "127.0.0.1", runtime.MediaDefaultPorts["tts"])
	image := service("image", "image", "127.0.0.1", runtime.MediaDefaultPorts["image"])
	videoService := service("video", "video", "127.0.0.1", runtime.MediaDefaultPorts["video"])
	for _, s := range []*runtime.Service{stt, tts, image, videoService} {
		if s != nil {
			recovery.Add(s)
			defer s.Close()
		}
	}
	videoJobs := newVideoJobs(videoService, r, log)

	access, err := auth.LoadAccess(r.Dir)
	if err != nil {
		return err
	}
	if !opts.NoAuth {
		// Without sign-in, nothing yet depends on who vouches for an address.
		auth.NameBindingIssuers(context.Background(), st, access, log)
	}
	browser := auth.NewBrowser(st, r.Dir, access, log)
	browser.Stylesheet = web.StylesheetURL() // its sign-in pages link the app's style sheet
	if browser.HasMagicLink() == nil {
		if err := auth.ClearMagicLinks(context.Background(), st); err != nil {
			log.Warn("Could not clear pending magic links.", "event", "auth.magic-links-clear-failed", "error", err.Error())
		}
	}
	for _, reg := range func() []auth.Registration {
		if access == nil {
			return nil
		}
		return access.Provider.Registrations
	}() {
		if reg.Kind == "github-oauth" {
			log.Warn("GitHub sign-in ("+reg.ID+") is not supported by this version and is ignored.", "event", "auth.registration-ignored")
		}
	}
	resolver := &auth.Resolver{Store: st, EnvKey: os.Getenv("LOCALBASE_API_KEY"), Mode: opts.AuthMode}
	gw := gateway.Config{Overrides: runningOverrides(opts.LLMHost, log), RunMemoryLimit: opts.RunMemoryLimit, RunsMemoryLimit: opts.RunsMemoryLimit, Store: st, Catalog: cat, Auth: resolver, LLM: llm, Log: log, NoAuth: opts.NoAuth, Placement: placementSource,
		MemoryGB: memoryGB, Root: r, Memory: mem, Browser: browser, Library: lib, STT: stt, TTS: tts, Image: image, Video: videoService, VideoJobs: videoJobs, Voices: voices.New(st, r.Dir), Otel: tel}
	gw.LogOutages = sink.Outages
	listenAddr := net.JoinHostPort(listenHost, strconv.Itoa(listenPort))
	// The UI asks the gateway who the caller is and calls its API, and the gateway is built with the
	// UI, so the UI reaches it through srv. gateway.New assigns it below, before the listener opens,
	// so no request can find it nil.
	var srv *gateway.Server
	ui := web.New(web.Deps{Store: st, Catalog: cat, LLM: llm, Auth: resolver, Memory: mem, NoAuth: opts.NoAuth, RootDir: r.Dir,
		Principal:     func(req *http.Request) *auth.Principal { p, _ := srv.BrowserPrincipal(req); return p },
		SignInEnabled: browser.Enabled(), LogoutURL: browser.CloudflareLogout(), Library: lib, Log: log,
		Call: func(ctx context.Context, p *auth.Principal, method, path, ct string, body []byte) web.APIResult {
			res := srv.Call(ctx, p, method, path, ct, body)
			return web.APIResult{Status: res.Status, Header: res.Header, Body: res.Body}
		},
		Voices: voices.New(st, r.Dir).List, ListenAddr: func() string { return listenAddr }})
	gw.UI, gw.LoginPages = ui, ui
	// Usage never stops the gateway: without it, nothing is counted.
	stopUsage := func() {}
	if rec, err := usage.New(st, log); err != nil {
		log.Warn("Usage totals are off: could not read their settings.", "event", "usage.start-failed", "error", err.Error())
	} else {
		gw.Usage = rec
		stopUsage = rec.Start()
	}
	// A start that fails from here on (a busy port, most often) stops what started; a gateway that
	// served stops them itself, in order, below.
	served := false
	defer func() {
		if !served {
			stopUsage()
			if videoJobs != nil {
				videoJobs.Shutdown()
			}
		}
	}()
	// The daily check is off by default, and this makes no request unless it is on.
	gw.Updates = upgrade.NewChecker(st, upgrade.DefaultSource(), version.Version, version.ReleaseBuild(), log)
	stopUpdates := gw.Updates.Start()
	defer stopUpdates()
	// A managed gateway that is ready acknowledges the static settings it
	// started with, clearing a matching pending restart (TS serve).
	if os.Getenv("LOCALBASE_SERVICE_ID") != "" && os.Getenv("LOCALBASE_SERVICE_TOKEN") != "" {
		started := &store.Config{GatewayHost: listenHost, GatewayPort: listenPort,
			SystemReserve: cfg.SystemReserve, AcceleratorReserve: cfg.AcceleratorReserve}
		gw.OnReady = func() {
			if err := st.AcknowledgeStatic(context.Background(), started); err != nil {
				log.Warn("Could not acknowledge the startup configuration.", "event", "config.acknowledge-failed", "error", err.Error())
			}
		}
	}
	srv = gateway.New(gw)

	ln, err := net.Listen("tcp", net.JoinHostPort(listenHost, strconv.Itoa(listenPort)))
	if err != nil {
		return err
	}
	if opts.NoAuth {
		if err := badNoAuth(netguard.NoAuthListener(ln.Addr())); err != nil {
			ln.Close()
			return err
		}
	}
	listenAddr = ln.Addr().String()
	ln = netlimit.LimitConnections(ln, connectionLimits(opts.MaxConnections, opts.MaxConnectionsPerIP, st), log)
	httpServer := &http.Server{Handler: srv} //nolint:gosec // Limits.Apply sets ReadHeaderTimeout on the next line (TestDefaultLimits)
	gw.Limits.Apply(httpServer)

	// Memory pressure: constrained stops idle runtimes; critical stops them all.
	stopPoll := make(chan struct{})
	feedDone := catalog.StartFeedScheduler(r.Dir, cat, log, stopPoll)
	monitor := runtime.NewPressureMonitor(mem, recovery, log)
	if videoJobs != nil {
		monitor.FailVideo = func() { videoJobs.FailActiveForMemoryPressure() }
	}
	go monitor.Run(stopPoll, runtime.PressurePollInterval)

	if gw.OnReady != nil && srv.Ready() {
		gw.OnReady()
	}
	baseURL := "http://" + ln.Addr().String()
	var loginLink string
	if firstRunLinkAllowed(opts.NoAuth, browser.Enabled(), os.Getenv("LOCALBASE_SERVICE_TOKEN")) {
		link, id, err := auth.FirstRunLink(context.Background(), st, ln.Addr().String())
		if id != "" {
			deps.Audit.LocalLinkIssued(id, false)
		}
		if err == nil {
			loginLink = link
		}
	}
	log.Info("LocalBase listening on "+baseURL, "event", "gateway.started", "root", r.Dir, "memory_gb", memoryGB, "auth", !opts.NoAuth, "version", version.Version, "commit", version.Get().Commit)
	enabled := map[string]bool{}
	for _, k := range store.Kinds {
		enabled[k] = k == "llm" || len(cfg.Selected[k]) > 0
	}
	rep.Started(baseURL, enabled, loginLink)
	errc := make(chan error, 1)
	go func() { errc <- httpServer.Serve(ln) }()
	served = true
	var serveErr error
	select {
	case s := <-sigs:
		signal.Stop(sigs) // a second signal ends the process at once, as one expects of a second Ctrl-C
		log.Info("Shutting down on "+s.String()+".", "event", "gateway.stopping")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("Listener failed.", "event", "gateway.error", "error", err.Error())
			serveErr = fmt.Errorf("listener failed: %w", err)
		}
	}
	srv.Stopping()
	// launchd and systemd kill at 15 s, and `upgrade` gives up on a stop then too. Everything below
	// is polite, one thing at a time, so this is what makes the limit true.
	stopStarted := time.Now()
	killBackends := time.AfterFunc(stopKillAt, func() {
		// Runs first, if nothing got that far: a killed backend would have them end as failed,
		// and what happened to them is that the gateway stopped.
		srv.StopRuns(time.Second)
		log.Error("The gateway ran out of time to stop, so every backend is being killed.", "event", "gateway.stop-deadline",
			"backends", runtime.KillAllBackends())
	})
	defer killBackends.Stop()
	giveUp := time.AfterFunc(stopExitAt, func() {
		log.Error("The gateway could not finish stopping in time and is exiting.", "event", "gateway.stop-abandoned")
		os.Exit(1)
	})
	defer giveUp.Stop()
	if opts.stopStuck != nil {
		opts.stopStuck()
	} else {
		labStopStuck()
	}
	// A load in flight goes first: everything below would otherwise wait on it.
	llm.StopLoads()
	for _, s := range []*runtime.Service{stt, tts, image, videoService} {
		if s != nil {
			s.StopLoads()
		}
	}
	srv.StopRuns(5 * time.Second) // runs end as interrupted, and their event streams close (DESIGN §21.6)
	close(stopPoll)
	<-feedDone // an in-flight refresh settles first
	ctx, cancel := context.WithDeadline(context.Background(), stopStarted.Add(stopDrainFor))
	defer cancel()
	httpServer.Shutdown(ctx)
	if videoJobs != nil {
		videoJobs.Shutdown() // the active job stops before its runtime closes
	}
	defer stopUsage()                   // after the requests and the jobs that it counts
	srv.StopManagement(3 * time.Second) // an accepted install gets a moment, then is interrupted and recorded
	lib.StopImports(3 * time.Second)    // what they downloaded stays, to resume
	llm.Close()
	// Every runtime is down before the log says the gateway is (the deferred
	// closes above then find nothing left to do).
	for _, s := range []*runtime.Service{stt, tts, image, videoService} {
		if s != nil {
			s.Close()
		}
	}
	srv.LogHeldBack() // nothing is coming now to bring it out
	_ = srv.Close()   // last: what New started (runs and management are stopped above); nil today
	log.Info("LocalBase stopped.", "event", "gateway.stopped")
	if serveErr != nil {
		return serveErr
	}
	rep.Stopped()
	return nil
}

// firstRunLinkAllowed: a service-managed gateway never prints one, since its
// captured stderr must never hold a link.
func firstRunLinkAllowed(noAuth, browserEnabled bool, serviceToken string) bool {
	return !noAuth && !browserEnabled && serviceToken == ""
}

// installer is the run's --install-missing hook: nil without consent, so the
// LLM manager answers 409 model_install_consent_required.
func installer(d Deps, allowed bool, log *slog.Logger) func(context.Context, string) error {
	if !allowed {
		return nil
	}
	return func(ctx context.Context, id string) error {
		log.Info("Installing selected model "+id+".", "event", "models.install-started", "model_id", id)
		_, err := install.ByID(ctx, install.ByIDDeps{Catalog: d.Catalog, Root: d.Root, Store: d.Store, Started: d.Config,
			Ineligible: func(s *catalog.Spec) string { return conf.Ineligible(s, d.Config.AllowExperimental) }}, id, func(ev install.Event) {
			if ev.Kind == "completed" {
				log.Info("Selected model "+id+" installation completed.", "event", "models.install-completed", "model_id", id)
			}
		})
		if err != nil {
			log.Error("Selected model "+id+" installation failed.", "event", "models.install-failed", "model_id", id, "error", err.Error())
		}
		return err
	}
}

// connectionLimits resolves the connection caps: the one asked for, else the configured
// value, else the default, fitted under the open file limit.
func connectionLimits(maxConns, maxPerIP *int, st *store.Store) netlimit.ConnLimits {
	resolve := func(asked *int, key string, fallback int) int {
		if asked != nil {
			return *asked
		}
		return savedConnLimit(st, key, fallback)
	}
	global := resolve(maxConns, store.MaxConnectionsKey, netlimit.DefaultConnLimits.Global)
	perIP := resolve(maxPerIP, store.MaxConnectionsPerIPKey, netlimit.DefaultConnLimits.PerIP)
	var files syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &files) == nil {
		global = netlimit.CapForFileLimit(global, files.Cur)
	}
	return netlimit.ConnLimits{Global: global, PerIP: perIP}
}

// savedConnLimit is a configured connection cap, or fallback when none is usable.
func savedConnLimit(st *store.Store, key string, fallback int) int {
	if v, ok, err := st.Setting(context.Background(), key); err == nil && ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}

// runningOverrides puts back what only this process has when a saved configuration is published:
// the environment and --llm-host, as serve applied them when it started. The environment was checked
// then, so an error here means it can't have changed; it is logged, and leaves that value saved.
func runningOverrides(llmHost string, log *slog.Logger) func(*store.Config) {
	return func(cfg *store.Config) {
		if err := conf.ApplyEnv(cfg); err != nil {
			log.Warn("Could not apply the environment to the running configuration.", "event", "config.override-failed", "error", err.Error())
		}
		if llmHost != "" {
			cfg.Host = llmHost
		}
	}
}
