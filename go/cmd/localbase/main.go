// Command localbase is the LocalBase gateway and CLI (installed as
// `local-base`). Milestone 0 implements `serve`, `keys create|list` and
// `version`; see go/DESIGN.md for the rest of the command surface.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/obs"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/web"
)

const usage = `LocalBase (Go rewrite, milestone 0)

Usage:
  local-base [--root DIR] serve [flags]
  local-base [--root DIR] keys create --name NAME [--scopes a,b] [--json]
  local-base [--root DIR] keys list [--json]
  local-base version
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// run returns the exit code: 0 ok, 1 operational failure, 2 invalid input.
func run(args []string) int {
	global := flag.NewFlagSet("local-base", flag.ContinueOnError)
	rootFlag := global.String("root", "", "data directory (default $LOCALBASE_ROOT or ~/.local/share/local-base)")
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := global.Parse(args); err != nil {
		return 2
	}
	rest := global.Args()
	if len(rest) == 0 {
		global.Usage()
		return 2
	}
	dir := root.Resolve(*rootFlag)
	switch rest[0] {
	case "serve":
		return serve(dir, rest[1:])
	case "keys":
		return keys(dir, rest[1:])
	case "version", "--version":
		fmt.Println(gateway.Version)
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n", rest[0])
	global.Usage()
	return 2
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}

func openRoot(dir string) (*root.Root, *store.Store, error) {
	r, err := root.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(r.DB())
	if err != nil {
		return nil, nil, err
	}
	return r, st, nil
}

func serve(dir string, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	host := fs.String("host", "", "gateway listen host (default from config)")
	port := fs.Int("port", 0, "gateway listen port (default from config)")
	llmPort := fs.Int("llm-port", 0, "llama-server port (default from config)")
	ctxSize := fs.Int("ctx-size", 0, "process-only LLM context override")
	noAuth := fs.Bool("no-auth", false, "disable API authentication")
	authMode := fs.String("auth-mode", "either", "bearer | x-api-key | either")
	queueCap := fs.Int("inference-queue-capacity", 16, "waiting requests per inference type")
	queueTimeout := fs.Int("inference-queue-timeout-ms", 60000, "queue wait deadline")
	bypass := fs.Bool("bypass-memory-check", false, "skip memory admission (unsafe)")
	logLevel := fs.String("log-level", "info", "debug | info | warn | error")
	devUILogin := fs.Bool("dev-ui-login", false, "milestone 0 only: let /app/login accept an API key with models:read")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *ctxSize == 0 {
		*ctxSize, _ = strconv.Atoi(os.Getenv("LOCALBASE_CTX_SIZE"))
	}
	r, st, err := openRoot(dir)
	if err != nil {
		return fail(err)
	}
	defer st.Close()
	cfg, err := st.LoadConfig(context.Background())
	if err != nil {
		return fail(err)
	}
	cat, err := catalog.Load(r.CatalogDir())
	if err != nil {
		return fail(err)
	}

	sink, err := obs.OpenSink(r.LogsDir())
	if err != nil {
		return fail(err)
	}
	defer sink.Close()
	if os.Getenv("LOCALBASE_SERVICE_TOKEN") == "" {
		sink.Mirror = os.Stderr // foreground serve also prints events
	}
	var level slog.Level
	level.UnmarshalText([]byte(*logLevel))
	log := slog.New(obs.NewHandler(sink, level))
	for _, d := range cat.Diagnostics {
		log.Warn("Skipped a catalog file: "+d.Message, "event", "catalog.entry-skipped", "path", d.Path)
	}

	listenHost, listenPort := cfg.GatewayHost, cfg.GatewayPort
	if *host != "" {
		listenHost = *host
	}
	if *port != 0 {
		listenPort = *port
	}
	release, err := r.Lock(listenHost, listenPort)
	if err != nil {
		return fail(err)
	}
	defer release()

	hostMem := memory.NewHostProvider()
	memoryGB := int(float64(hostMem.Topology().SystemCapacity)/float64(memory.GiB) + 0.5)
	mem := memory.NewController(hostMem, memory.Reserve{Percent: cfg.SystemReserve.Percent, MinimumGB: cfg.SystemReserve.MinimumGB}, *bypass)
	llm := runtime.NewLLM(runtime.LLMDeps{Root: r.Dir, ModelsDir: r.ModelsDir("llm"), RuntimesDir: r.RuntimesDir(),
		Store: st, Catalog: cat, Memory: mem, Log: log, MemoryGB: float64(memoryGB), CtxOverride: *ctxSize,
		PortOverride: *llmPort, QueueCapacity: *queueCap, QueueTimeout: time.Duration(*queueTimeout) * time.Millisecond,
		Binary: os.Getenv("LOCALBASE_LLAMA_SERVER")}, cfg)
	defer llm.Close()

	resolver := &auth.Resolver{Store: st, EnvKey: os.Getenv("LOCALBASE_API_KEY"), Mode: *authMode}
	srv := &gateway.Server{Store: st, Catalog: cat, Auth: resolver, LLM: llm, Log: log, NoAuth: *noAuth, MemoryGB: memoryGB}
	srv.UI = web.New(web.Deps{Store: st, Catalog: cat, LLM: llm, Auth: resolver, Memory: mem,
		NoAuth: *noAuth, DevLogin: *devUILogin, RootDir: r.Dir})
	gateway.New(srv)

	ln, err := net.Listen("tcp", net.JoinHostPort(listenHost, strconv.Itoa(listenPort)))
	if err != nil {
		return fail(err)
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

	log.Info(fmt.Sprintf("LocalBase listening on http://%s", ln.Addr()), "event", "gateway.started",
		"root", r.Dir, "memory_gb", memoryGB, "auth", !*noAuth)
	errc := make(chan error, 1)
	go func() { errc <- httpServer.Serve(ln) }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case s := <-sigs:
		log.Info("Shutting down on "+s.String()+".", "event", "gateway.stopping")
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("Listener failed.", "event", "gateway.error", "error", err.Error())
		}
	}
	srv.Stopping()
	close(stopPoll)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)
	llm.Close()
	log.Info("LocalBase stopped.", "event", "gateway.stopped")
	return 0
}

func keys(dir string, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("keys "+args[0], flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON envelope output")
	name := fs.String("name", "", "key name")
	scopes := fs.String("scopes", strings.Join(auth.DefaultKeyScopes, ","), "comma-separated permissions")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	_, st, err := openRoot(dir)
	if err != nil {
		return fail(err)
	}
	defer st.Close()
	out := func(data any, human func(io.Writer)) int {
		if *asJSON {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": true, "data": data})
		} else {
			human(os.Stdout)
		}
		return 0
	}
	switch args[0] {
	case "create":
		list := []string{}
		for _, s := range strings.Split(*scopes, ",") {
			if s = strings.TrimSpace(s); s != "" {
				if !contains(auth.Permissions, s) {
					fmt.Fprintf(os.Stderr, "invalid scope %q\n", s)
					return 2
				}
				list = append(list, s)
			}
		}
		k, raw, err := st.CreateAPIKey(context.Background(), *name, auth.Canonical(list))
		if err != nil {
			return fail(err)
		}
		return out(map[string]any{"id": k.ID, "name": k.Name, "scopes": k.Scopes, "key": raw}, func(w io.Writer) {
			fmt.Fprintf(w, "Created %s (%s). Store this key now; it is shown once:\n%s\n", k.Name, k.ID, raw)
		})
	case "list":
		ks, err := st.ListAPIKeys(context.Background())
		if err != nil {
			return fail(err)
		}
		var rows []map[string]any
		for _, k := range ks {
			rows = append(rows, map[string]any{"id": k.ID, "name": k.Name, "prefix": k.Prefix, "scopes": k.Scopes,
				"createdAt": k.CreatedAt, "expiresAt": k.ExpiresAt, "revokedAt": k.RevokedAt})
		}
		return out(rows, func(w io.Writer) {
			for _, k := range ks {
				fmt.Fprintf(w, "%s  %-24s %s\n", k.ID, k.Name, strings.Join(k.Scopes, ","))
			}
		})
	}
	fmt.Fprintf(os.Stderr, "unknown keys command %q\n", args[0])
	return 2
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
