package main

import (
	"errors"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/cli"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
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

func serve(c *cli.Ctx) (any, error) {
	opts, err := serveOptions(c)
	if err != nil {
		return nil, err
	}
	open := func() (Deps, func(), error) {
		e, err := openEnv(c)
		if err != nil {
			return Deps{}, nil, err
		}
		return Deps{Root: e.Root, Store: e.Store, Config: e.Config, Catalog: e.Catalog, Host: e.Host, Audit: e.audit}, e.Close, nil
	}
	err = Run(opts, open, serveReporter{c})
	var bad *BadInput
	if errors.As(err, &bad) {
		return nil, cli.Inputf("%s", bad.Message)
	}
	return nil, err
}

// serveOptions checks serve's flags, before anything starts.
func serveOptions(c *cli.Ctx) (Options, error) {
	opts := Options{AllowRoot: c.Bool("allow-root"), Host: c.String("host"), LLMHost: c.String("llm-host"),
		LLMModelFile: c.String("llm-model-file"), InstallMissing: c.Bool("install-missing"),
		BypassMemoryCheck: c.Bool("bypass-memory-check"), EventsToStderr: !c.Global.JSON}
	// What slog reads is accepted as before (any case); the rest would quietly have been info.
	if v := c.String("log-level"); v != "" {
		if err := opts.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return Options{}, cli.Inputf(`log-level: Invalid option: expected one of "debug"|"info"|"warn"|"error"`)
		}
	}
	var err error
	if opts.Port, err = intOpt(c, "port", 1, 65535); err != nil {
		return Options{}, err
	}
	if opts.LLMPort, err = intOpt(c, "llm-port", 1, 65535); err != nil {
		return Options{}, err
	}
	if opts.PortBase, err = intOpt(c, "runtime-port-base", 1, 65535-(len(runtime.PortOrder)-1)); err != nil {
		return Options{}, err
	}
	if !c.Has("llm-port") {
		opts.LLMPort = runtime.PortInBlock(opts.PortBase, "llm", opts.LLMPort)
	}
	if opts.CtxSize, err = intOpt(c, "ctx-size", 2048, 2_147_483_647); err != nil {
		return Options{}, err
	}
	if opts.QueueCapacity, err = intOpt(c, "inference-queue-capacity", 1, 10_000); err != nil {
		return Options{}, err
	}
	queueTimeout, err := intOpt(c, "inference-queue-timeout-ms", 1, 3_600_000)
	if err != nil {
		return Options{}, err
	}
	opts.QueueTimeout = time.Duration(queueTimeout) * time.Millisecond
	runMemory, err := intOpt(c, "run-memory-limit-mib", 1, 1<<20)
	if err != nil {
		return Options{}, err
	}
	runsMemory, err := intOpt(c, "runs-memory-limit-mib", 1, 1<<20)
	if err != nil {
		return Options{}, err
	}
	opts.RunMemoryLimit, opts.RunsMemoryLimit = int64(runMemory)<<20, int64(runsMemory)<<20
	opts.AuthMode = c.String("auth-mode")
	if opts.AuthMode == "" {
		opts.AuthMode = "either"
	}
	if !contains([]string{"bearer", "x-api-key", "either"}, opts.AuthMode) {
		return Options{}, cli.Inputf("Invalid --auth-mode value: %s. Expected bearer|x-api-key|either", opts.AuthMode)
	}
	authOn, authGiven := c.OptBool("auth")
	opts.NoAuth = authGiven && !authOn
	// A media runtime that is off takes no host or port, as before.
	opts.Media = map[string]MediaOptions{}
	for _, name := range []string{"stt", "tts", "image", "video"} {
		m := MediaOptions{Host: c.String(name + "-host")}
		if enabled, set := c.OptBool(name); set && !enabled {
			m = MediaOptions{Off: true}
		} else if c.Has(name + "-port") {
			if m.Port, err = intOpt(c, name+"-port", 1, 65535); err != nil {
				return Options{}, err
			}
		}
		opts.Media[name] = m
	}
	for _, limit := range []struct {
		flag string
		to   **int
	}{{"max-connections", &opts.MaxConnections}, {"max-connections-per-ip", &opts.MaxConnectionsPerIP}} {
		if c.Has(limit.flag) {
			n, err := intOpt(c, limit.flag, 0, 10_000_000)
			if err != nil {
				return Options{}, err
			}
			*limit.to = &n
		}
	}
	return opts, nil
}

// serveReporter is serve's output: a line for each under --json, else what a person reads.
type serveReporter struct{ c *cli.Ctx }

func (r serveReporter) Streaming() {
	if r.c.Global.JSON {
		r.c.Streaming()
	}
}

func (r serveReporter) Started(baseURL string, enabled map[string]bool, loginLink string) {
	if r.c.Global.JSON {
		line := map[string]any{"event": "started", "baseUrl": baseURL, "enabled": enabled}
		if loginLink != "" {
			line["loginLink"] = loginLink
		}
		r.c.Line(line)
		return
	}
	r.c.Info("LocalBase gateway listening on %s", baseURL)
	printLoginLink(r.c, loginLink)
}

func (r serveReporter) Stopped() {
	if r.c.Global.JSON {
		r.c.Line(map[string]any{"event": "stopped", "exitCode": 0})
	}
}
