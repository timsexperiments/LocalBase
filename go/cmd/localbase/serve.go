package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/backends"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/gateway"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/web"
)

func serveCommand() *cli.Command {
	opt := func(name, value, help string) cli.Option { return cli.Option{Name: name, Value: value, Help: help} }
	flag := func(name, help string) cli.Option { return cli.Option{Name: name, Bool: true, Help: help} }
	return &cli.Command{Name: "serve", Summary: "Start the unified LocalBase API gateway", Options: []cli.Option{
		flag("install-missing", "Download selected models that are missing"),
		opt("host", "<host>", "Gateway host"), opt("port", "<port>", "Gateway port"),
		flag("llm", "Enable the LLM service (--no-llm disables it)"),
		flag("stt", "Enable the STT service (--no-stt disables it)"),
		flag("tts", "Enable speech generation (--no-tts disables it)"),
		flag("image", "Enable image generation (--no-image disables it)"),
		flag("video", "Enable video generation runtime (--no-video disables it)"),
		opt("llm-host", "<host>", "llama-server host"), opt("llm-port", "<port>", "llama-server port"),
		opt("stt-host", "<host>", "whisper-server host"), opt("stt-port", "<port>", "whisper-server port"),
		opt("image-host", "<host>", "sd-server host"), opt("image-port", "<port>", "sd-server port"),
		opt("video-host", "<host>", "video sd-server host"), opt("video-port", "<port>", "video sd-server port"),
		opt("tts-host", "<host>", "tts-server host"), opt("tts-port", "<port>", "tts-server port"),
		opt("ctx-size", "<tokens>", "LLM context limit"),
		opt("inference-queue-capacity", "<requests>", "Maximum waiting requests per inference type"),
		opt("inference-queue-timeout-ms", "<milliseconds>", "Maximum inference queue wait"),
		opt("stt-path", "<path>", "Whisper endpoint path"),
		opt("llm-model-file", "<file>", "LLM model filename override"),
		opt("stt-model-file", "<file>", "STT model filename override"),
		opt("tts-model-file", "<file>", "TTS backbone model filename override"),
		opt("image-model-file", "<file>", "Image model filename override"),
		flag("auth", "Enable API key authentication (--no-auth disables it)"),
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

func serve(c *cli.Ctx) (any, error) {
	port, err := intOpt(c, "port", 1, 65535)
	if err != nil {
		return nil, err
	}
	llmPort, err := intOpt(c, "llm-port", 1, 65535)
	if err != nil {
		return nil, err
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

	e, err := openEnv(c)
	if err != nil {
		return nil, err
	}
	defer e.Close()
	r, st, cfg, cat := e.Root, e.Store, e.Config, e.Catalog
	if err := applyEnvOverrides(cfg); err != nil {
		return nil, err
	}

	if os.Getenv("LOCALBASE_SERVICE_TOKEN") != "" {
		obs.BoundServiceStderr(r.Dir) // #357: launchd never rotates the capture
	}
	sink, err := obs.OpenSink(r.LogsDir())
	if err != nil {
		return nil, err
	}
	defer sink.Close()
	if os.Getenv("LOCALBASE_SERVICE_TOKEN") == "" && !c.Global.JSON {
		sink.Mirror = os.Stderr // foreground serve also prints events
	}
	var level slog.Level
	level.UnmarshalText([]byte(c.String("log-level")))
	log := slog.New(obs.NewHandler(sink, level))
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
	// Hold the operation lease (or claim the one a service start handed off)
	// until this process owns the gateway lock.
	initLease, err := root.ServeInitialization(r.Dir, os.Getenv("LOCALBASE_SERVICE_TOKEN"))
	if err != nil {
		return nil, err
	}
	release, err := r.LockAs(listenHost, listenPort, os.Getenv("LOCALBASE_SERVICE_ID"), os.Getenv("LOCALBASE_SERVICE_TOKEN"))
	initLease.Release()
	if err != nil {
		return nil, err
	}
	defer release()

	memoryGB := e.memoryGB()
	mem := memory.NewController(e.Host, memory.Reserve{Percent: cfg.SystemReserve.Percent, MinimumGB: cfg.SystemReserve.MinimumGB},
		c.Bool("bypass-memory-check"))
	lib, err := library.Open(r, st, cat, backends.ProbeAccelerators(backends.ProbeOptions{}))
	if err != nil {
		return nil, err
	}
	for _, d := range lib.Registry.Skipped() {
		log.Warn("Skipped a custom backend definition: "+d, "event", "backend.definition-skipped")
	}
	installMissing := c.Bool("install-missing") || cfg.InstallMissingModels
	var llm *runtime.LLM
	// A switch to another configured model makes it the active one, as in TS.
	activate := func(kind, modelID string) {
		cur := llm.Config()
		if cur.Active[kind] == modelID {
			return
		}
		next := cur.Clone()
		next.Active[kind] = modelID
		if err := root.WithOperation(r.Dir, "update runtime configuration", func() error { return st.SaveConfig(context.Background(), next) }); err != nil {
			log.Warn("Could not record the active "+kind+" model.", "event", "config.save-failed", "error", err.Error())
			return
		}
		llm.SetConfig(next)
	}
	llm = runtime.NewLLM(runtime.LLMDeps{Root: r.Dir, ModelsDir: r.ModelsDir("llm"), RuntimesDir: r.RuntimesDir(),
		Store: st, Catalog: cat, Memory: mem, Log: log, MemoryGB: float64(memoryGB), CtxOverride: ctxSize,
		PortOverride: llmPort, QueueCapacity: queueCap, QueueTimeout: time.Duration(queueTimeout) * time.Millisecond,
		Binary: os.Getenv("LOCALBASE_LLAMA_SERVER"), ModelFileOverride: c.String("llm-model-file"),
		Install: installer(e, installMissing, log), Gate: lib.Gate(), SelectionKey: lib.SelectionKey, Activate: activate}, cfg)
	defer llm.Close()
	stt := runtime.NewService(runtime.ServiceDeps{Modality: "stt", Kind: "stt", Catalog: cat, Store: st, Memory: mem, Log: log,
		Config: llm.Config, Gate: lib.GateFor("transcription"), SelectionKey: lib.SelectionKey, RuntimesDir: r.RuntimesDir(),
		ModelsDir: r.ModelsDir("stt"), CacheDir: filepath.Join(r.Dir, "cache"), Host: cfg.SttHost, Port: cfg.SttPort,
		Binaries: map[string]string{"whisper-server": os.Getenv("LOCALBASE_WHISPER_SERVER")}, Install: installer(e, installMissing, log), Activate: activate})
	defer stt.Close()

	access, err := auth.LoadAccess(r.Dir)
	if err != nil {
		return nil, err
	}
	browser := auth.NewBrowser(st, r.Dir, access, log)
	if browser.HasMagicLink() == nil {
		auth.ClearMagicLinks(context.Background(), st)
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
	srv := &gateway.Server{Store: st, Catalog: cat, Auth: resolver, LLM: llm, Log: log, NoAuth: noAuth,
		MemoryGB: memoryGB, Root: r, Memory: mem, Browser: browser, Library: lib, STT: stt}
	srv.UI = web.New(web.Deps{Store: st, Catalog: cat, LLM: llm, Auth: resolver, Memory: mem, NoAuth: noAuth, RootDir: r.Dir,
		Principal:     func(req *http.Request) *auth.Principal { p, _ := srv.BrowserPrincipal(req); return p },
		SignInEnabled: browser.Enabled(), LogoutURL: browser.CloudflareLogout(), Library: lib})
	gateway.New(srv)

	ln, err := net.Listen("tcp", net.JoinHostPort(listenHost, strconv.Itoa(listenPort)))
	if err != nil {
		return nil, err
	}
	httpServer := &http.Server{Handler: srv, ReadHeaderTimeout: 30 * time.Second}

	// Memory pressure: a critical sample stops the runtime and rejects queued work.
	stopPoll := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopPoll:
				return
			case <-t.C:
				if mem.Poll(context.Background()) == "emergency-stop" {
					log.Error("Memory is critical; stopping runtimes.", "event", "memory.emergency-stop")
					llm.EmergencyStop(&memory.AdmissionError{Reason: "memory-pressure", PoolID: "system"})
				}
			}
		}
	}()

	baseURL := "http://" + ln.Addr().String()
	log.Info("LocalBase listening on "+baseURL, "event", "gateway.started", "root", r.Dir, "memory_gb", memoryGB, "auth", !noAuth)
	if c.Global.JSON {
		enabled := map[string]bool{}
		for _, k := range store.Kinds {
			enabled[k] = k == "llm" || len(cfg.Selected[k]) > 0
		}
		c.Line(map[string]any{"event": "started", "baseUrl": baseURL, "enabled": enabled})
	} else {
		c.Info("LocalBase gateway listening on %s", baseURL)
	}
	errc := make(chan error, 1)
	go func() { errc <- httpServer.Serve(ln) }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	var serveErr error
	select {
	case s := <-sigs:
		log.Info("Shutting down on "+s.String()+".", "event", "gateway.stopping")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("Listener failed.", "event", "gateway.error", "error", err.Error())
			serveErr = fmt.Errorf("listener failed: %w", err)
		}
	}
	srv.Stopping()
	close(stopPoll)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
	llm.Close()
	log.Info("LocalBase stopped.", "event", "gateway.stopped")
	if serveErr != nil {
		return nil, serveErr
	}
	if c.Global.JSON {
		c.Line(map[string]any{"event": "stopped", "exitCode": 0})
	}
	return nil, nil
}
