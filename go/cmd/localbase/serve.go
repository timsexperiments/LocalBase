package main

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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/gateway"
	"github.com/timsexperiments/LocalBase/go/internal/jsjson"
	"github.com/timsexperiments/LocalBase/go/internal/lease"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/otlp"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/upgrade"
	"github.com/timsexperiments/LocalBase/go/internal/usage"
	"github.com/timsexperiments/LocalBase/go/internal/version"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
	"github.com/timsexperiments/LocalBase/go/internal/web"
)

// nameBindingIssuers gives older email bindings and invitations their sign-in provider
// when only one could be meant (DESIGN §13.6, item 5b), and says what is left. It never
// stops a start: what names no provider grants nothing, and the host commands still work.
func nameBindingIssuers(st *store.Store, access *auth.BrowserAccess, log *slog.Logger) {
	ctx := context.Background()
	adopted, err := auth.AdoptBindingIssuers(ctx, st, access)
	if err != nil {
		log.Error("Naming the sign-in provider on older bindings failed; they grant nothing until it is done.", "event", "auth.issuers-adopt-failed", "error", err.Error())
	} else if adopted > 0 {
		log.Info("Email bindings and invitations now name their sign-in provider.", "event", "auth.issuers-adopted", "count", adopted)
	}
	left, err := auth.CountUnbound(ctx, st.DB)
	if err != nil || len(access.Issuers()) == 0 {
		return // with no provider, nobody can sign in and there is nothing to choose
	}
	for _, group := range []struct {
		kind, what string
		count      int
	}{{"email", "email bindings", left.Emails}, {"email-domain", "domain bindings", left.Domains}, {"invitation", "invitations", left.Invitations}} {
		if group.count > 0 {
			log.Warn(fmt.Sprintf("%d %s are not bound to a sign-in provider, so they grant nothing. Run: local-base access bindings adopt --issuer <issuer>", group.count, group.what),
				"event", "auth.bindings-unbound", "kind", group.kind, "count", group.count)
		}
	}
}

// stopStuck does nothing outside a localbase_lab build, where it can stand for a stop that never
// returns, to test what happens then.
var stopStuck = func() {}

// How long a stop gets. Requests drain until stopDrainFor, every backend is killed at stopKillAt
// if any is left, and the process leaves at stopExitAt whatever is still going: all inside the
// 15 s launchd, systemd and `upgrade` allow.
const (
	stopDrainFor = 9 * time.Second
	stopKillAt   = 12 * time.Second
	stopExitAt   = 13500 * time.Millisecond
)

func serveCommand() *cli.Command {
	opt := func(name, value, help string) cli.Option { return cli.Option{Name: name, Value: value, Help: help} }
	flag := func(name, help string) cli.Option { return cli.Option{Name: name, Bool: true, Help: help} }
	return &cli.Command{Name: "serve", Summary: "Start the unified LocalBase API gateway", Options: []cli.Option{
		flag("install-missing", "Download selected models that are missing"),
		flag("allow-root", "Serve even as root, in a container that holds nothing else"),
		opt("host", "<host>", "Gateway host"), opt("port", "<port>", "Gateway port"),
		flag("llm", "Enable the LLM service (--no-llm disables it)"),
		flag("stt", "Enable the STT service (--no-stt disables it)"),
		flag("tts", "Enable speech generation (--no-tts disables it)"),
		flag("image", "Enable image generation (--no-image disables it)"),
		flag("video", "Enable video generation runtime (--no-video disables it)"),
		opt("runtime-port-base", "<port>", "First of five backend ports in a row: LLM, STT, TTS, image, video (for a second gateway on this computer). A --<runtime>-port flag wins"),
		opt("llm-host", "<host>", "llama-server host"), opt("llm-port", "<port>", "llama-server port"),
		opt("stt-host", "<host>", "whisper-server host"), opt("stt-port", "<port>", "whisper-server port"),
		opt("image-host", "<host>", "sd-server host"), opt("image-port", "<port>", "sd-server port"),
		opt("video-host", "<host>", "video sd-server host"), opt("video-port", "<port>", "video sd-server port"),
		opt("tts-host", "<host>", "tts-server host"), opt("tts-port", "<port>", "tts-server port"),
		opt("ctx-size", "<tokens>", "LLM context limit"),
		opt("inference-queue-capacity", "<requests>", "Maximum waiting requests per inference type"),
		opt("inference-queue-timeout-ms", "<milliseconds>", "Maximum inference queue wait"),
		opt("run-memory-limit-mib", "<MiB>", "Most one detached chat run may hold in memory (default 32)"),
		opt("runs-memory-limit-mib", "<MiB>", "Most all retained detached runs may hold in memory together (default 256)"),
		opt("max-connections", "<connections>", "Maximum open connections, 0 for no cap (default: the configured value, or 4096)"),
		opt("max-connections-per-ip", "<connections>", "Maximum open connections from one address other than loopback, 0 for no cap (default: the configured value, or 256)"),
		opt("stt-path", "<path>", "Whisper endpoint path"),
		opt("llm-model-file", "<file>", "LLM model filename override"),
		opt("stt-model-file", "<file>", "STT model filename override"),
		opt("tts-model-file", "<file>", "TTS backbone model filename override"),
		opt("image-model-file", "<file>", "Image model filename override"),
		flag("auth", "Enable API key authentication (--no-auth disables it: local development only, on loopback, never tunneled)"),
		opt("auth-mode", "<bearer|x-api-key|either>", "Authentication header mode"),
		flag("bypass-memory-check", "Start despite model memory warnings"),
		opt("log-level", "<debug|info|warn|error>", "Minimum event severity (default info)"),
	}, Run: serve}
}

func intOpt(c *cli.Ctx, name string, lo, hi int) (int, error) {
	n, ok, err := c.Int(name)
	if err != nil {
		return 0, err
	}
	if ok && (n < lo || n > hi) {
		return 0, cli.Inputf("--%s must be from %d to %d", name, lo, hi)
	}
	return n, nil
}

// afterSignals runs once serve listens for stop signals. It exists only so a test can send one at
// that point.
var afterSignals = func() {}

// mediaDefaultPorts are the speech, image and video backends' ports with no flag and no base. upgrade's
// survivor check reads them too.
var mediaDefaultPorts = map[string]int{"tts": 8092, "image": 8090, "video": 8091}

// runtimePortOrder is the order of --runtime-port-base's block.
var runtimePortOrder = []string{"llm", "stt", "tts", "image", "video"}

// runtimePort is a runtime's port in the block from base, or configured with no base.
func runtimePort(base int, runtime string, configured int) int {
	if base <= 0 {
		return configured
	}
	return base + slices.Index(runtimePortOrder, runtime)
}

// effectiveUID is a variable so a test can be root without being root.
var effectiveUID = os.Geteuid

func serve(c *cli.Ctx) (any, error) {
	if effectiveUID() == 0 && !c.Bool("allow-root") {
		return nil, cli.Inputf("Refusing to serve as root: a gateway runs the backends it downloads and reads model files from anywhere, and all of that would run as root. Run it as an ordinary user, or pass --allow-root in a container that holds nothing else.")
	}
	// Checked before anything starts. What slog reads is accepted as before (any case); the rest
	// would quietly have been info.
	var level slog.Level
	if v := c.String("log-level"); v != "" {
		if err := level.UnmarshalText([]byte(v)); err != nil {
			return nil, cli.Inputf(`log-level: Invalid option: expected one of "debug"|"info"|"warn"|"error"`)
		}
	}
	port, err := intOpt(c, "port", 1, 65535)
	if err != nil {
		return nil, err
	}
	llmPort, err := intOpt(c, "llm-port", 1, 65535)
	if err != nil {
		return nil, err
	}
	portBase, err := intOpt(c, "runtime-port-base", 1, 65535-(len(runtimePortOrder)-1))
	if err != nil {
		return nil, err
	}
	if !c.Has("llm-port") {
		llmPort = runtimePort(portBase, "llm", llmPort)
	}
	ctxSize, err := intOpt(c, "ctx-size", 2048, 2_147_483_647)
	if err != nil {
		return nil, err
	}
	queueCap, err := intOpt(c, "inference-queue-capacity", 1, 10_000)
	if err != nil {
		return nil, err
	}
	queueTimeout, err := intOpt(c, "inference-queue-timeout-ms", 1, 3_600_000)
	if err != nil {
		return nil, err
	}
	runMemory, err := intOpt(c, "run-memory-limit-mib", 1, 1<<20)
	if err != nil {
		return nil, err
	}
	runsMemory, err := intOpt(c, "runs-memory-limit-mib", 1, 1<<20)
	if err != nil {
		return nil, err
	}
	authMode := c.String("auth-mode")
	if authMode == "" {
		authMode = "either"
	}
	if !contains([]string{"bearer", "x-api-key", "either"}, authMode) {
		return nil, cli.Inputf("Invalid --auth-mode value: %s. Expected bearer|x-api-key|either", authMode)
	}
	authOn, authGiven := c.OptBool("auth")
	noAuth := authGiven && !authOn
	if c.Global.JSON {
		c.Streaming()
	}

	// From here on a stop signal waits its turn: it is read once startup is done, and everything
	// started so far is closed by its defer. Before, a SIGTERM during startup ended the process
	// with none of them.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	afterSignals()

	e, err := openEnv(c)
	if err != nil {
		return nil, err
	}
	defer e.Close()
	r, st, cfg, cat := e.Root, e.Store, e.Config, e.Catalog
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, err
	}

	// Before anything writes to them: a planted link must not redirect a log (TS).
	if problems := obs.CheckRootFiles(r.Dir); len(problems) > 0 {
		// A service manager retries this: the event is once per cause, and stderr
		// (the returned error) says it every time.
		if obs.RecordStartRefusal(r.Dir, problems[0], time.Now()) {
			obs.LogStartRefusal(r.Dir, r.LogsDir(), problems[0])
		}
		return nil, problems[0]
	}
	obs.ClearStartRefusal(r.Dir)
	if os.Getenv("LOCALBASE_SERVICE_TOKEN") != "" {
		if err := obs.BoundServiceStderr(r.Dir); err != nil { // #357: launchd never rotates the capture
			var unsafe *obs.UnsafeFileError
			if errors.As(err, &unsafe) {
				return nil, err
			}
		}
	}
	sink, err := obs.OpenSink(r.LogsDir())
	if err != nil {
		return nil, err
	}
	defer sink.Close()
	if os.Getenv("LOCALBASE_SERVICE_TOKEN") == "" && !c.Global.JSON {
		sink.Mirror = os.Stderr // foreground serve also prints events
	}
	handler := obs.NewHandler(sink, level)
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
	if h := c.String("host"); h != "" {
		listenHost = h
	}
	if port != 0 {
		listenPort = port
	}
	if h := c.String("llm-host"); h != "" {
		cfg.Host = h
	}
	if noAuth {
		if err := noAuthHostError(listenHost); err != nil {
			return nil, err
		}
	}
	// Hold the operation lease (or claim the one a service start handed off)
	// until this process owns the gateway lock.
	initLease, err := lease.ServeInitialization(r.Dir, os.Getenv("LOCALBASE_SERVICE_TOKEN"))
	if err != nil {
		return nil, err
	}
	release, err := r.LockAs(listenHost, listenPort, os.Getenv("LOCALBASE_SERVICE_ID"), os.Getenv("LOCALBASE_SERVICE_TOKEN"))
	initLease.Release()
	if err != nil {
		return nil, err
	}
	// With the root's lock held, whatever an earlier gateway left running is not
	// serving anyone: it goes before this one starts backends on the same ports.
	runtime.SweepBackends(r.Dir, log)
	defer release()
	tel, err := startTelemetry(cfg, handler, log)
	if err != nil {
		return nil, err
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
		if v, ok := memory.ProbeGPUWorkingSet(ctx, e.Host, bin); ok {
			gpuLimitLogged.Do(func() {
				log.Info("Read the GPU memory limit from Metal.", "event", "memory.gpu-limit", "gpu_limit_bytes", v, "source", "metal")
			})
		}
	}
	go probeGPU()
	memoryGB := e.memoryGB()
	mem := memory.NewController(e.Host, memory.Reserve{Percent: cfg.SystemReserve.Percent, MinimumGB: cfg.SystemReserve.MinimumGB},
		c.Bool("bypass-memory-check"))
	lib, err := library.Open(r, st, cat, backends.ProbeAccelerators(backends.ProbeOptions{}))
	if err != nil {
		return nil, err
	}
	lib.Log = log
	for _, d := range lib.Registry.Skipped() {
		log.Warn("Skipped a custom backend definition: "+d, "event", "backend.definition-skipped")
	}
	installMissing := c.Bool("install-missing") || cfg.InstallMissingModels
	// A model refused for memory may stop the idle runtimes of other modalities.
	recovery := &runtime.IdleRecovery{Memory: mem, Log: log}
	var llm *runtime.LLM
	// CPU/GPU placement per model on this device (DESIGN §20).
	placementSource := &runtime.HostPlacement{Store: st, DeviceID: lib.DeviceID, Memory: mem, Log: log}
	// A switch to another configured model makes it the active one, as in TS.
	activate := func(kind, modelID string) {
		if err := recordActive(r.Dir, st, llm, kind, modelID); err != nil {
			log.Warn("Could not record the active "+kind+" model.", "event", "config.save-failed", "error", err.Error())
		}
	}
	llm = runtime.NewLLM(runtime.LLMDeps{Root: r.Dir, ModelsDir: r.ModelsDir("llm"), RuntimesDir: r.RuntimesDir(),
		Store: st, Catalog: cat, Memory: mem, Log: log, MemoryGB: float64(memoryGB), CtxOverride: ctxSize,
		PortOverride: llmPort, QueueCapacity: queueCap, QueueTimeout: time.Duration(queueTimeout) * time.Millisecond,
		Binary: os.Getenv("LOCALBASE_LLAMA_SERVER"), ModelFileOverride: c.String("llm-model-file"),
		BeforeSwitch: probeGPU, Install: installer(e, installMissing, log), Gate: lib.Gate(), SelectionKey: lib.SelectionKey, Activate: activate, Otel: tel, Recovery: recovery,
		Placement: placementSource, GPUPool: &memory.GPUPool{Reserve: placement.ReserveBytes}}, cfg)
	recovery.Add(llm)
	defer llm.Close()
	// The media modalities: one supervised backend each, through the selection.
	lib.InstallMissing = installMissing
	lib.HFToken = func() string { return llm.Config().HFToken }
	hostPort := func(name, host string, port int) (string, int, error) {
		if h := c.String(name + "-host"); h != "" {
			host = h
		}
		if c.Has(name + "-port") {
			p, err := intOpt(c, name+"-port", 1, 65535)
			if err != nil {
				return "", 0, err
			}
			return host, p, nil
		}
		return host, runtimePort(portBase, name, port), nil
	}
	binaries := map[string]string{"whisper-server": os.Getenv("LOCALBASE_WHISPER_SERVER"), "tts-server": os.Getenv("LOCALBASE_TTS_SERVER"),
		"crispasr": os.Getenv("LOCALBASE_CRISPASR"), "sd-server": os.Getenv("LOCALBASE_SD_SERVER")}
	service := func(modality, task, host string, port int) (*runtime.Service, error) {
		if enabled, set := c.OptBool(modality); set && !enabled {
			return nil, nil
		}
		h, p, err := hostPort(modality, host, port)
		if err != nil {
			return nil, err
		}
		d := runtime.ServiceDeps{Modality: modality, Kind: modality, Catalog: cat, Store: st, Memory: mem, Log: log,
			Config: llm.Config, Gate: lib.GateFor(task), SelectionKey: lib.SelectionKey, RuntimesDir: r.RuntimesDir(),
			ModelsDir: r.ModelsDir(modality), RootDir: r.Dir, CacheDir: filepath.Join(r.Dir, "cache"), Host: h, Port: p,
			Binaries: binaries, Install: installer(e, installMissing, log), Activate: activate,
			QueueCapacity: queueCap, QueueTimeout: time.Duration(queueTimeout) * time.Millisecond, Otel: tel, Recovery: recovery}
		if modality == "tts" {
			// A changed voice set or marking setting restarts the speech server.
			d.ExtraKey = func() string {
				return voiceStamp(st) + "|marking=" + strconv.FormatBool(runtime.SpeechMarking(context.Background(), st))
			}
		}
		return runtime.NewService(d), nil
	}
	stt, err := service("stt", "transcription", cfg.SttHost, cfg.SttPort)
	if err != nil {
		return nil, err
	}
	tts, err := service("tts", "speech", "127.0.0.1", mediaDefaultPorts["tts"])
	if err != nil {
		return nil, err
	}
	image, err := service("image", "image", "127.0.0.1", mediaDefaultPorts["image"])
	if err != nil {
		return nil, err
	}
	videoService, err := service("video", "video", "127.0.0.1", mediaDefaultPorts["video"])
	if err != nil {
		return nil, err
	}
	for _, s := range []*runtime.Service{stt, tts, image, videoService} {
		if s != nil {
			recovery.Add(s)
			defer s.Close()
		}
	}
	videoJobs := newVideoJobs(videoService, r, log)

	access, err := auth.LoadAccess(r.Dir)
	if err != nil {
		return nil, err
	}
	if !noAuth {
		// Without sign-in, nothing yet depends on who vouches for an address.
		nameBindingIssuers(st, access, log)
	}
	browser := auth.NewBrowser(st, r.Dir, access, log)
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
	resolver := &auth.Resolver{Store: st, EnvKey: os.Getenv("LOCALBASE_API_KEY"), Mode: authMode}
	srv := &gateway.Server{Overrides: runningOverrides(c.String("llm-host"), log), RunMemoryLimit: int64(runMemory) << 20, RunsMemoryLimit: int64(runsMemory) << 20, Store: st, Catalog: cat, Auth: resolver, LLM: llm, Log: log, NoAuth: noAuth, Placement: placementSource,
		MemoryGB: memoryGB, Root: r, Memory: mem, Browser: browser, Library: lib, STT: stt, TTS: tts, Image: image, Video: videoService, VideoJobs: videoJobs, Voices: voices.New(st, r.Dir), Otel: tel}
	srv.LogOutages = sink.Outages
	listenAddr := net.JoinHostPort(listenHost, strconv.Itoa(listenPort))
	ui := web.New(web.Deps{Store: st, Catalog: cat, LLM: llm, Auth: resolver, Memory: mem, NoAuth: noAuth, RootDir: r.Dir,
		Principal:     func(req *http.Request) *auth.Principal { p, _ := srv.BrowserPrincipal(req); return p },
		SignInEnabled: browser.Enabled(), LogoutURL: browser.CloudflareLogout(), Library: lib, Log: log,
		Call: func(ctx context.Context, p *auth.Principal, method, path, ct string, body []byte) web.APIResult {
			res := srv.Call(ctx, p, method, path, ct, body)
			return web.APIResult{Status: res.Status, Header: res.Header, Body: res.Body}
		},
		Voices: voices.New(st, r.Dir).List, ListenAddr: func() string { return listenAddr }})
	srv.UI, srv.LoginPages = ui, ui
	// Usage never stops the gateway: without it, nothing is counted.
	stopUsage := func() {}
	if rec, err := usage.New(st, log); err != nil {
		log.Warn("Usage totals are off: could not read their settings.", "event", "usage.start-failed", "error", err.Error())
	} else {
		srv.Usage = rec
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
	srv.Updates = upgrade.NewChecker(st, upgrade.DefaultSource(), version.Version, releaseBuild(), log)
	stopUpdates := srv.Updates.Start()
	defer stopUpdates()
	gateway.New(srv)
	// A managed gateway that is ready acknowledges the static settings it
	// started with, clearing a matching pending restart (TS serve).
	if os.Getenv("LOCALBASE_SERVICE_ID") != "" && os.Getenv("LOCALBASE_SERVICE_TOKEN") != "" {
		started := &store.Config{GatewayHost: listenHost, GatewayPort: listenPort,
			SystemReserve: cfg.SystemReserve, AcceleratorReserve: cfg.AcceleratorReserve}
		srv.OnReady = func() {
			if err := st.AcknowledgeStatic(context.Background(), started); err != nil {
				log.Warn("Could not acknowledge the startup configuration.", "event", "config.acknowledge-failed", "error", err.Error())
			}
		}
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(listenHost, strconv.Itoa(listenPort)))
	if err != nil {
		return nil, err
	}
	if noAuth {
		if err := noAuthListenerError(ln.Addr()); err != nil {
			ln.Close()
			return nil, err
		}
	}
	listenAddr = ln.Addr().String()
	connLimits, err := connectionLimits(c, st)
	if err != nil {
		ln.Close()
		return nil, err
	}
	ln = gateway.LimitConnections(ln, connLimits, log)
	httpServer := &http.Server{Handler: srv} //nolint:gosec // Limits.Apply sets ReadHeaderTimeout on the next line (TestDefaultLimits)
	srv.Limits.Apply(httpServer)

	// Memory pressure: constrained stops idle runtimes; critical stops them all.
	stopPoll := make(chan struct{})
	feedDone := startFeedScheduler(r.Dir, cat, log, stopPoll)
	monitor := runtime.NewPressureMonitor(mem, recovery, log)
	if videoJobs != nil {
		monitor.FailVideo = func() { videoJobs.FailActiveForMemoryPressure() }
	}
	go monitor.Run(stopPoll, runtime.PressurePollInterval)

	if srv.OnReady != nil && srv.Ready() {
		srv.OnReady()
	}
	baseURL := "http://" + ln.Addr().String()
	var loginLink string
	if firstRunLinkAllowed(noAuth, browser.Enabled(), os.Getenv("LOCALBASE_SERVICE_TOKEN")) {
		loginLink = firstRunLoginLink(e, ln.Addr().String())
	}
	log.Info("LocalBase listening on "+baseURL, "event", "gateway.started", "root", r.Dir, "memory_gb", memoryGB, "auth", !noAuth, "version", version.Version, "commit", version.Get().Commit)
	if c.Global.JSON {
		enabled := map[string]bool{}
		for _, k := range store.Kinds {
			enabled[k] = k == "llm" || len(cfg.Selected[k]) > 0
		}
		line := map[string]any{"event": "started", "baseUrl": baseURL, "enabled": enabled}
		if loginLink != "" {
			line["loginLink"] = loginLink
		}
		c.Line(line)
	} else {
		c.Info("LocalBase gateway listening on %s", baseURL)
		printLoginLink(c, loginLink)
	}
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
	stopStuck()
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
	log.Info("LocalBase stopped.", "event", "gateway.stopped")
	if serveErr != nil {
		return nil, serveErr
	}
	if c.Global.JSON {
		c.Line(map[string]any{"event": "stopped", "exitCode": 0})
	}
	return nil, nil
}

// connectionLimits resolves the connection caps: a flag, else the configured
// value, else the default, fitted under the open file limit.
func connectionLimits(c *cli.Ctx, st *store.Store) (gateway.ConnLimits, error) {
	resolve := func(flag, key string, fallback int) (int, error) {
		if c.Has(flag) {
			return intOpt(c, flag, 0, 10_000_000)
		}
		return savedConnLimit(st, key, fallback), nil
	}
	global, err := resolve("max-connections", store.MaxConnectionsKey, gateway.DefaultConnLimits.Global)
	if err != nil {
		return gateway.ConnLimits{}, err
	}
	perIP, err := resolve("max-connections-per-ip", store.MaxConnectionsPerIPKey, gateway.DefaultConnLimits.PerIP)
	if err != nil {
		return gateway.ConnLimits{}, err
	}
	var files syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &files) == nil {
		global = gateway.CapForFileLimit(global, files.Cur)
	}
	return gateway.ConnLimits{Global: global, PerIP: perIP}, nil
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

// voiceStamp identifies the stored custom voice set; the warm speech server
// restarts when it changes, since it loads voices at launch.
func voiceStamp(st *store.Store) string {
	voices, err := st.Voices(context.Background())
	if err != nil {
		return "error"
	}
	var b strings.Builder
	for _, v := range voices {
		b.WriteString(v.ID + ":" + v.SHA256 + ";")
	}
	return b.String()
}

// startFeedScheduler keeps the remote catalog feed current (TS
// feed-scheduler): one check at start, then every hour; each check downloads
// only when the cached copy is older than a day. An update reloads the
// catalog in place. The returned channel closes once stopped.
func startFeedScheduler(rootDir string, cat *catalog.Catalog, log *slog.Logger, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	lastError := ""
	check := func() {
		res := catalog.RefreshFeed(context.Background(), rootDir, catalog.RefreshOptions{})
		short := func(s string) string { return obs.BoundedText(s, 512) }
		switch res.Outcome {
		case "updated":
			lastError = ""
			next, err := catalog.LoadRoot(rootDir, nil)
			if err != nil {
				log.Warn("The updated catalog could not be loaded; the previous one stays in use.", "event", "catalog_feed.reload_failed",
					"category", "gateway", "component", "catalog-feed", "runtime", "gateway", slog.Group("attributes", "error", short(err.Error())))
				return
			}
			cat.Replace(next)
			log.Info("Loaded a new remote catalog feed.", "event", "catalog_feed.updated", "category", "gateway",
				"component", "catalog-feed", "runtime", "gateway", slog.Group("attributes", "url", short(feedURLForDiagnostics(res.URL))))
		case "failed":
			if res.Error == lastError {
				return
			}
			lastError = res.Error
			log.Warn("The remote catalog feed could not be refreshed; the last good copy stays in use.", "event", "catalog_feed.refresh_failed",
				"category", "gateway", "component", "catalog-feed", "runtime", "gateway",
				slog.Group("attributes", "url", short(feedURLForDiagnostics(res.URL)), "error", short(res.Error)))
		default:
			lastError = ""
		}
	}
	go func() {
		defer close(done)
		check()
		t := time.NewTicker(catalog.FeedCheckInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				check()
			}
		}
	}()
	return done
}

// startTelemetry starts OTLP export when OTEL_* or the saved settings
// enable it (TS activateContextOtel): every event the gateway logs is
// exported with its span correlation, except the exporter's own
// diagnostics, which stay in the local log.
func startTelemetry(cfg *store.Config, handler *obs.Handler, log *slog.Logger) (*otlp.Runtime, error) {
	oc, err := obs.ResolveOtel(cfg.OtelEndpoint, cfg.OtelHeaders, cfg.OtelSampleRatio, os.Getenv)
	if err != nil {
		return nil, err
	}
	tel := otlp.New(oc, version.Version, func(severity, message string, attrs map[string]any) {
		event, level := "observability.export-failed", slog.LevelWarn
		switch severity {
		case "info":
			event, level = "observability.export-recovered", slog.LevelInfo
		case "error":
			level = slog.LevelError
		}
		args := []any{"event", event, "category", "logging", "component", "otel", obs.LocalOnly, true}
		keys := make([]string, 0, len(attrs))
		for k := range attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, k, attrs[k])
		}
		log.Log(context.Background(), level, message, args...)
	})
	if tel.Enabled() {
		hooks := handler.Hooks()
		hooks.Correlate = otlp.Correlation
		hooks.Export = func(line []byte) {
			if v, err := jsjson.Parse(line); err == nil {
				if o, ok := v.(*jsjson.Object); ok {
					tel.Emit(o)
				}
			}
		}
	}
	return tel, nil
}

// runningOverrides puts back what only this process has when a saved configuration is published:
// the environment and --llm-host, as serve applied them when it started. The environment was checked
// then, so an error here means it can't have changed; it is logged, and leaves that value saved.
func runningOverrides(llmHost string, log *slog.Logger) func(*store.Config) {
	return func(cfg *store.Config) {
		if err := applyEnvOverrides(cfg); err != nil {
			log.Warn("Could not apply the environment to the running configuration.", "event", "config.override-failed", "error", err.Error())
		}
		if llmHost != "" {
			cfg.Host = llmHost
		}
	}
}
