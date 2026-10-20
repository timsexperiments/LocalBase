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
	"strconv"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/gateway"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
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
		flag("dev-ui-login", "Milestone 0 only: let /app/login accept an API key with models:read"),
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
	release, err := r.Lock(listenHost, listenPort)
	if err != nil {
		return nil, err
	}
	defer release()

	memoryGB := e.memoryGB()
	mem := memory.NewController(e.Host, memory.Reserve{Percent: cfg.SystemReserve.Percent, MinimumGB: cfg.SystemReserve.MinimumGB},
		c.Bool("bypass-memory-check"))
	installMissing := c.Bool("install-missing") || cfg.InstallMissingModels
	llm := runtime.NewLLM(runtime.LLMDeps{Root: r.Dir, ModelsDir: r.ModelsDir("llm"), RuntimesDir: r.RuntimesDir(),
		Store: st, Catalog: cat, Memory: mem, Log: log, MemoryGB: float64(memoryGB), CtxOverride: ctxSize,
		PortOverride: llmPort, QueueCapacity: queueCap, QueueTimeout: time.Duration(queueTimeout) * time.Millisecond,
		Binary: os.Getenv("LOCALBASE_LLAMA_SERVER"), ModelFileOverride: c.String("llm-model-file"),
		Install: installer(e, installMissing, log)}, cfg)
	defer llm.Close()

	resolver := &auth.Resolver{Store: st, EnvKey: os.Getenv("LOCALBASE_API_KEY"), Mode: authMode}
	srv := &gateway.Server{Store: st, Catalog: cat, Auth: resolver, LLM: llm, Log: log, NoAuth: noAuth,
		MemoryGB: memoryGB, Root: r, Memory: mem}
	srv.UI = web.New(web.Deps{Store: st, Catalog: cat, LLM: llm, Auth: resolver, Memory: mem,
		NoAuth: noAuth, DevLogin: c.Bool("dev-ui-login"), RootDir: r.Dir})
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
