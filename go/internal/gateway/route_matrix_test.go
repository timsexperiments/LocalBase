package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/memory"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
	"github.com/timsexperiments/LocalBase/go/internal/web"
)

// Access rules for every route the gateway and the UI register. Routes are read
// from the source, so a route added later is tested here without touching this
// file, and it counts as protected until it is listed below on purpose.

// publicRoutes serve a request with no credential. Keep this list short.
var publicRoutes = map[string]string{
	"/health":                   "liveness probe; no model ids",
	"/health/ready":             "readiness probe",
	"/{$}":                      "redirects to the app",
	"GET /{$}":                  "redirects to the app",
	"/app":                      "redirects to the app",
	"GET /app":                  "redirects to the app",
	"/app/":                     "the UI's routes; unmatched paths are not found",
	"GET /app/assets/{name...}": "static files built into the binary",
	"/openapi.json":             "the same static document everywhere, no instance data (DESIGN 30.1)",
}

// signInRoutes are the browser layer's own paths: sign-in pages and callbacks,
// which must work before anyone is signed in, and the deny-by-default gate in
// front of /app/api. Which of them answer depends on browser access.
var signInRoutes = []string{"/app/login", "/oidc/callback", "/github/callback", "/app/login/magic", "/magic-link/callback",
	"/app/logout", "/app/session", "/app/api", "/app/api/"}

// tokenRoutes answer only the local CLI, which proves itself with the instance
// token; anything else gets the same empty 404.
var tokenRoutes = []string{"/_localbase/instance", "/_localbase/default-model", "/_localbase/placement-state"}

// matrixExtraPaths are more paths for routes that dispatch on the rest of the path.
var matrixExtraPaths = map[string][]string{
	"/_localbase/models/{model_id...}": {chatModel + "/placement", chatModel + "/unload", "no-such-model", "no-such-model/placement", "no-such-model/unload"},
	"/_localbase/runs/{rest...}":       {"v1/chat/completions", "v1/audio/transcriptions", "v1/audio/translations", "run_x", "run_x/events", "run_x/cancel", "run_x/result"},
	"/v1/models/{model...}":            {"no-such-model"},
	"/_localbase/voices/":              {"voice-000000000000", "voice-000000000000/audio", "not-a-voice"},
	"/app/api/": {"_localbase/api-keys", "_localbase/access-management", "_localbase/hf-token", "_localbase/model-imports",
		"_localbase/runs", "_localbase/runs/run_x/events", "_localbase/models/" + chatModel + "/unload", "v1/chat/completions", "v1/audio/transcriptions",
		"v1/audio/translations", "v1/videos"},
}

var matrixMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
	http.MethodOptions, http.MethodTrace, http.MethodConnect}

// registeredPatterns reads the patterns a source file registers on a mux
// variable, including patterns registered from a range over a string list.
func registeredPatterns(t *testing.T, file, mux string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.RangeStmt:
			lit, ok := x.X.(*ast.CompositeLit)
			val, isIdent := x.Value.(*ast.Ident)
			if !ok || !isIdent {
				return true
			}
			registers := false
			ast.Inspect(x.Body, func(b ast.Node) bool {
				if c, ok := b.(*ast.CallExpr); ok {
					if s, ok := c.Fun.(*ast.SelectorExpr); ok && isMuxCall(s, mux) && len(c.Args) > 0 {
						if id, ok := c.Args[0].(*ast.Ident); ok && id.Name == val.Name {
							registers = true
						}
					}
				}
				return true
			})
			if registers {
				for _, e := range lit.Elts {
					if b, ok := e.(*ast.BasicLit); ok {
						s, _ := strconv.Unquote(b.Value)
						out = append(out, s)
					}
				}
			}
		case *ast.CallExpr:
			if s, ok := x.Fun.(*ast.SelectorExpr); ok && isMuxCall(s, mux) && len(x.Args) > 0 {
				if b, ok := x.Args[0].(*ast.BasicLit); ok {
					s, _ := strconv.Unquote(b.Value)
					out = append(out, s)
				}
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("no routes found in %s: has registration changed shape?", file)
	}
	return out
}

func isMuxCall(s *ast.SelectorExpr, mux string) bool {
	id, ok := s.X.(*ast.Ident)
	return ok && id.Name == mux && (s.Sel.Name == "HandleFunc" || s.Sel.Name == "Handle")
}

var patternParam = regexp.MustCompile(`\{([a-z_]+)(\.\.\.)?\}`)

// patternPath is a concrete path for a pattern, without its method.
func patternPath(pattern string) string {
	if _, rest, ok := strings.Cut(pattern, " "); ok {
		pattern = rest
	}
	pattern = strings.ReplaceAll(pattern, "{$}", "")
	return patternParam.ReplaceAllStringFunc(pattern, func(p string) string {
		switch name := patternParam.FindStringSubmatch(p)[1]; name {
		case "model", "model_id":
			return chatModel
		case "job_id":
			return "00000000-0000-0000-0000-000000000000"
		case "tool":
			return "image"
		case "rest":
			return "run_x"
		case "name":
			return "app.css"
		default:
			return "x"
		}
	})
}

func gatewayPatterns(t *testing.T) []string { return tablePatterns(t, "routes.go") }

// tablePatterns reads the route table's patterns from its source, so that every route is seen,
// whatever a running server registers: each pattern: "..." in the file.
func tablePatterns(t *testing.T, file string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "pattern" {
				if b, ok := kv.Value.(*ast.BasicLit); ok {
					s, _ := strconv.Unquote(b.Value)
					out = append(out, s)
				}
			}
		}
		return true
	})
	if len(out) == 0 {
		t.Fatalf("no routes found in %s: has the table changed shape?", file)
	}
	return out
}
func uiPatterns(t *testing.T) []string { return registeredPatterns(t, "../web/web.go", "m") }

// matrixGateway is a whole gateway with authentication on: API routes, browser
// routes and UI pages. body counts the request body bytes read by anything.
type matrixGateway struct {
	ts       *httptest.Server
	srv      *Server
	stt      *runtime.Service
	llm      *runtime.LLM
	store    *store.Store
	bodyRead *atomic.Int64
	noScopes string // a key with no permissions
	patterns []string
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !fromRejectionDrain() {
		b.n.Add(int64(n))
	}
	return n, err
}

// fromRejectionDrain reports a read by Error, which discards the rest of a
// refused request's body. It is the one read that is not a handler using the body.
func fromRejectionDrain() bool {
	pcs := make([]uintptr, 24)
	frames := goruntime.CallersFrames(pcs[:goruntime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "httpx.Error") {
			return true
		}
		if !more {
			return false
		}
	}
}

func newMatrixGateway(t *testing.T, access *auth.BrowserAccess) *matrixGateway {
	t.Helper()
	ts, stt, _ := newAuthSTTGateway(t)
	ts.Close()
	base := ts.Config.Handler.(*Server)
	dir := scratchDir(t)
	r, err := root.Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	models := r.ModelsDir("llm")
	os.MkdirAll(models, 0o700)
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		t.Fatal(err)
	}
	installSparse(t, models, cat.ByID(chatModel))
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, noScopes, err := st.CreateAPIKey(context.Background(), "no-scopes", []string{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &store.Config{Host: "127.0.0.1", Port: freePort(t), CtxSize: 8192, CtxMode: "auto", Parallel: "1",
		Active: map[string]string{"llm": chatModel, "stt": sttModel}, Selected: map[string][]string{"llm": {chatModel}, "stt": {sttModel}}}
	cfg.Root, cfg.GatewayHost, cfg.SttHost = r.Dir, "127.0.0.1", "127.0.0.1"
	if err := st.SaveConfig(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	mem := memory.NewController(host{}, memory.Reserve{Percent: 15, MinimumGB: 8}, false)
	mem.SetSampler(nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	llm := runtime.NewLLM(runtime.LLMDeps{Root: r.Dir, ModelsDir: models, Store: st, Catalog: cat, Memory: mem,
		Log: log, MemoryGB: 64, Binary: fakeBinary, QueueCapacity: 4, QueueTimeout: 5 * time.Second}, cfg)
	t.Cleanup(llm.Close)
	browser := auth.NewBrowser(st, r.Dir, access, log)
	resolver := &auth.Resolver{Store: st, Mode: "either"}
	srv := &Server{Store: st, Catalog: cat, Auth: resolver, LLM: llm, Log: log, MemoryGB: 64, Root: r, Memory: mem,
		Browser: browser, STT: base.STT, Voices: voices.New(st, r.Dir),
		Placement: &runtime.HostPlacement{Store: st, DeviceID: "test", Memory: mem, Log: log}}
	srv.UI = web.New(web.Deps{Store: st, Catalog: cat, LLM: llm, Auth: resolver, Memory: mem, RootDir: r.Dir,
		Principal:     func(req *http.Request) *auth.Principal { p, _ := srv.BrowserPrincipal(req); return p },
		SignInEnabled: browser.Enabled(), LogoutURL: browser.CloudflareLogout(),
		Voices: voices.New(st, r.Dir).List})
	New(srv)
	bodyRead := &atomic.Int64{}
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Body != nil {
			req.Body = countingBody{req.Body, bodyRead}
		}
		srv.ServeHTTP(w, req)
	}))
	t.Cleanup(probe.Close)
	return &matrixGateway{ts: probe, srv: srv, stt: stt, llm: llm, store: st, bodyRead: bodyRead, noScopes: noScopes, patterns: uiPatterns(t)}
}

// runsLeft is how many runs are unfinished, and how many runs have a checkpoint in the gateway's runs
// directory. A run that is kept is saved from when it's queued, or as it fails if it fails first,
// and a run refused before the queue is forgotten, so together they see every run, finished or not.
func (g *matrixGateway) runsLeft(t *testing.T) (active, saved int) {
	t.Helper()
	if g.srv.Root == nil {
		t.Fatal("the gateway has no root, so its runs are never saved")
	}
	entries, err := os.ReadDir(filepath.Join(g.srv.Root.Dir, "runs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return g.srv.runs.ActiveCount(), len(entries)
}

// matrixBody is a well-formed form that every handler that reads a body would read.
func matrixBody() (string, string) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	fw, _ := w.CreateFormFile("file", "a.wav")
	fw.Write([]byte("RIFF0000WAVEfmt "))
	w.WriteField("model", sttModel)
	w.Close()
	return b.String(), w.FormDataContentType()
}

type rawResponse struct {
	status   int
	body     string
	location string
	allow    string
}

// raw sends a request byte for byte, so that a method or target that clients
// refuse to send still reaches the server.
func (g *matrixGateway) raw(t *testing.T, method, target, hostHeader string, headers map[string]string, body string) rawResponse {
	t.Helper()
	conn, err := net.DialTimeout("tcp", g.ts.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(20 * time.Second))
	if hostHeader == "" {
		hostHeader = g.ts.Listener.Addr().String()
	}
	var req strings.Builder
	fmt.Fprintf(&req, "%s %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n", method, target, hostHeader)
	for k, v := range headers {
		fmt.Fprintf(&req, "%s: %s\r\n", k, v)
	}
	if body != "" {
		fmt.Fprintf(&req, "Content-Length: %d\r\n", len(body))
	}
	req.WriteString("\r\n" + body)
	if _, err := conn.Write([]byte(req.String())); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("%s %s: reading the response: %v", method, target, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return rawResponse{status: resp.StatusCode, body: string(b), location: resp.Header.Get("Location"), allow: resp.Header.Get("Allow")}
}

// send is raw with the matrix's form body, on methods that can carry one.
func (g *matrixGateway) send(t *testing.T, method, target, hostHeader string, headers map[string]string) rawResponse {
	t.Helper()
	body, ct := "", ""
	if method != http.MethodConnect && method != http.MethodTrace && method != http.MethodHead {
		body, ct = matrixBody()
	}
	h := map[string]string{}
	if ct != "" {
		h["Content-Type"] = ct
	}
	for k, v := range headers {
		h[k] = v
	}
	return g.raw(t, method, target, hostHeader, h, body)
}

// matrixPaths lists every concrete path of a set of patterns.
func matrixPaths(patterns []string) map[string]string {
	paths := map[string]string{}
	for _, p := range patterns {
		path := patternPath(p)
		if path != "" {
			paths[path] = p
		} else {
			paths["/"] = p
		}
		for _, extra := range matrixExtraPaths[p] {
			base := strings.TrimSuffix(patternPath(strings.SplitN(p, "{", 2)[0]), "/")
			paths[base+"/"+extra] = p
		}
	}
	return paths
}

// pageHasMethod reports whether a UI pattern for path takes method (GET also serves HEAD).
func pageHasMethod(patterns []string, path, method string) bool {
	for _, p := range patterns {
		m, _, ok := strings.Cut(p, " ")
		if ok && patternPath(p) == path && (m == method || (m == http.MethodGet && method == http.MethodHead)) {
			return true
		}
	}
	return false
}

func isSignInPath(path string) bool {
	if strings.HasPrefix(path, "/app/api") {
		return true
	}
	return slices.Contains(signInRoutes, path)
}

func isPublicPath(path string) bool {
	switch {
	case path == "/health", path == "/health/ready", path == "/", path == "/app", path == "/app/", path == "/openapi.json":
		return true
	case strings.HasPrefix(path, "/app/assets/"):
		return true
	}
	return false
}

// checkAnonymous is what a request with no credential may get.
func checkAnonymous(t *testing.T, g *matrixGateway, method, path, label string, r rawResponse, browser bool) {
	t.Helper()
	bad := func(want string) {
		t.Errorf("%s %s%s: %d %.120q, want %s", method, path, label, r.status, r.body, want)
	}
	isUIPage := strings.HasPrefix(path, "/app/") && !isSignInPath(path) && !isPublicPath(path)
	switch {
	case method == http.MethodOptions && !isSignInPath(path):
		if r.status != 403 {
			bad("403")
		}
	case isPublicPath(path):
		// Public routes may answer, but only with what they exist for.
		if !slices.Contains([]int{200, 301, 302, 307, 404, 405, 503}, r.status) {
			bad("a public route's answer")
		}
	case slices.Contains(tokenRoutes, path):
		if r.status != 404 {
			bad("404")
		}
	case isSignInPath(path):
		// Sign-in pages answer when browser access is on; everything else is denied.
		allowed := []int{401, 403, 404, 405}
		if browser {
			allowed = append(allowed, 200, 303)
		}
		if !slices.Contains(allowed, r.status) {
			bad(fmt.Sprintf("one of %v", allowed))
		}
		if strings.HasPrefix(path, "/app/api") && r.status != 401 && r.status != 403 && r.status != 404 {
			bad("401, 403 or 404")
		}
	case isUIPage:
		switch {
		case !pageHasMethod(g.patterns, path, method):
			// The UI's catch-all answers a method the page does not register.
			if r.status != 404 && r.status != 405 {
				bad("404 or 405")
			}
		case browser && r.status == 303:
			if r.location != "/app/login" {
				bad("a redirect to /app/login")
			}
		case !browser && r.status == 401:
		default:
			bad("sign-in")
		}
	default:
		if r.status != 401 && r.status != 405 {
			bad("401 or 405")
		}
	}
}

// bodyAllowed: routes that may read a body from someone with no credential.
func bodyAllowed(method, path string, browser bool) bool {
	return browser && method == http.MethodPost && (path == "/app/login/magic" || path == "/magic-link/callback")
}

func directAccess() *auth.BrowserAccess {
	return &auth.BrowserAccess{Origin: "https://lb.example", Permissions: []string{"models:read"},
		Provider: auth.Provider{Kind: "direct", Registrations: []auth.Registration{
			{Kind: "magic-link", ID: "email", Name: "Email"},
			{Kind: "oidc", ID: "idp", Name: "IdP", Issuer: "https://idp.invalid", ClientID: "c", ClientAuthentication: &auth.ClientAuth{Kind: "none"}},
		}}}
}

func TestRouteClassesNameRealRoutes(t *testing.T) {
	all := append(gatewayPatterns(t), uiPatterns(t)...)
	for p := range publicRoutes {
		if !slices.Contains(all, p) {
			t.Errorf("publicRoutes lists %q, which is not registered", p)
		}
	}
	for _, p := range append(slices.Clone(signInRoutes), tokenRoutes...) {
		if !slices.Contains(all, p) {
			t.Errorf("%q is listed but not registered", p)
		}
	}
	for path := range matrixPaths(all) {
		if isPublicPath(path) && !slices.ContainsFunc(all, func(p string) bool { _, ok := publicRoutes[p]; return ok && patternPathMatches(p, path) }) {
			t.Errorf("%s is treated as public but no public route matches it", path)
		}
	}
}

func patternPathMatches(pattern, path string) bool {
	return patternPath(pattern) == path || (patternPath(pattern) == "" && path == "/")
}

// No request without a credential reaches a handler that is not public, whatever
// its method, and nothing it sends is read.
func TestAnonymousRouteMatrix(t *testing.T) {
	for _, mode := range []struct {
		name   string
		access *auth.BrowserAccess
		host   string
	}{{"no browser access", nil, ""}, {"direct browser access", directAccess(), "lb.example"}} {
		t.Run(mode.name, func(t *testing.T) {
			g := newMatrixGateway(t, mode.access)
			browser := mode.access != nil
			paths := matrixPaths(append(gatewayPatterns(t), uiPatterns(t)...))
			if len(paths) < 80 {
				t.Fatalf("only %d paths: route discovery looks broken", len(paths))
			}
			for path := range paths {
				for _, method := range matrixMethods {
					g.bodyRead.Store(0)
					r := g.send(t, method, path, mode.host, nil)
					checkAnonymous(t, g, method, path, "", r, browser)
					if n := g.bodyRead.Load(); n != 0 && !bodyAllowed(method, path, browser) {
						t.Errorf("%s %s: %d request body bytes were read before authentication", method, path, n)
					}
				}
			}
			g.checkNoSideEffects(t)
		})
	}
}

// checkNoSideEffects: nothing the matrix sent started a runtime or created a run.
func (g *matrixGateway) checkNoSideEffects(t *testing.T) {
	t.Helper()
	if s := g.stt.Snapshot().State; s != "idle" {
		t.Errorf("a refused request changed the speech-to-text runtime: %q", s)
	}
	if s := g.llm.Snapshot().State; s != "idle" {
		t.Errorf("a refused request changed the LLM runtime: %q", s)
	}
	if active, saved := g.runsLeft(t); active != 0 || saved != 0 {
		t.Errorf("runs exist after refused requests: %d unfinished, %d saved", active, saved)
	}
	keys, err := g.store.ListAPIKeys(context.Background())
	if err != nil || len(keys) != 1 {
		t.Errorf("API keys changed after refused requests: %d, %v", len(keys), err)
	}
}

// A request's method is the method on its request line: no header or field changes it.
func TestMethodOverridesChangeNothing(t *testing.T) {
	g := newMatrixGateway(t, nil)
	paths := matrixPaths(append(gatewayPatterns(t), uiPatterns(t)...))
	overrides := []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodOptions}
	for path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			base := g.send(t, method, path, "", nil)
			for _, to := range overrides {
				for _, h := range []string{"X-HTTP-Method-Override", "X-Method-Override", "X-HTTP-Method"} {
					r := g.send(t, method, path, "", map[string]string{h: to})
					if r.status != base.status {
						t.Errorf("%s %s with %s: %s: %d, without it %d", method, path, h, to, r.status, base.status)
					}
				}
				sep := "?"
				if strings.Contains(path, "?") {
					sep = "&"
				}
				r := g.send(t, method, path+sep+"_method="+to, "", nil)
				if r.status != base.status {
					t.Errorf("%s %s?_method=%s: %d, without it %d", method, path, to, r.status, base.status)
				}
				if method == http.MethodPost {
					form := "_method=" + to
					r := g.raw(t, method, path, "", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, form)
					if r.status != 405 && r.status != base.status {
						t.Errorf("%s %s with a _method=%s field: %d, without it %d", method, path, to, r.status, base.status)
					}
				}
			}
		}
	}
	g.checkNoSideEffects(t)
}

// An unauthenticated caller learns nothing about which models exist.
func TestAnonymousCannotTellWhichModelsExist(t *testing.T) {
	g := newMatrixGateway(t, nil)
	for _, form := range []string{"/_localbase/models/%s", "/_localbase/models/%s/placement", "/_localbase/models/%s/unload", "/v1/models/%s"} {
		for _, method := range matrixMethods {
			known := g.send(t, method, fmt.Sprintf(form, chatModel), "", nil)
			unknown := g.send(t, method, fmt.Sprintf(form, "no-such-model"), "", nil)
			if known.status != unknown.status || known.body != unknown.body {
				t.Errorf("%s %s: a real model gets %d %q, an unknown one %d %q", method, form, known.status, known.body, unknown.status, unknown.body)
			}
		}
	}
}

// A key with no permissions is refused like anyone else, except on routes that
// only ever show the caller's own work.
func TestKeyWithoutPermissionsRouteMatrix(t *testing.T) {
	g := newMatrixGateway(t, nil)
	for path := range matrixPaths(gatewayPatterns(t)) {
		if isPublicPath(path) || isSignInPath(path) || slices.Contains(tokenRoutes, path) {
			continue
		}
		for _, method := range matrixMethods {
			r := g.send(t, method, path, "", map[string]string{"Authorization": "Bearer " + g.noScopes})
			ownWork := strings.HasPrefix(path, "/_localbase/runs")
			switch {
			case method == http.MethodOptions:
				if r.status != 403 {
					t.Errorf("%s %s: %d, want 403", method, path, r.status)
				}
			case ownWork:
				if r.status >= 200 && r.status < 300 && !(path == "/_localbase/runs" && (method == http.MethodGet || method == http.MethodHead)) {
					t.Errorf("%s %s: %d %.200q", method, path, r.status, r.body)
				}
			case strings.HasSuffix(path, "/not-a-voice") || path == "/_localbase/voices/":
				// Not a voice route at all: the same answer as any unknown path.
				if r.status != 404 {
					t.Errorf("%s %s: %d %.200q, want 404", method, path, r.status, r.body)
				}
			case path == "/openapi.json" && (method == http.MethodGet || method == http.MethodHead):
				// Public (DESIGN 30.1).
				if r.status != 200 {
					t.Errorf("%s %s: %d, want 200", method, path, r.status)
				}
			case path == "/_localbase/usage" && method == http.MethodGet:
				// Any key reads its own usage (DESIGN 32); this gateway records none.
				if r.status != 501 {
					t.Errorf("%s %s: %d %.200q, want 501", method, path, r.status, r.body)
				}
			case r.status != 403 && r.status != 405:
				t.Errorf("%s %s: %d %.200q, want 403 or 405", method, path, r.status, r.body)
			}
		}
	}
	g.checkNoSideEffects(t)
}

// A cancel goes through a route the matrix already covers, but with a body it never sends: its own
// action. Stopping an install or an import changes what this computer does, so each one needs
// models:manage, and a request refused here stops nothing.
func TestCancellingNeedsModelsManage(t *testing.T) {
	cancels := []struct {
		name, path, body string
		// What someone allowed gets, with nothing to stop.
		allowed int
	}{
		{"an install", "/_localbase/model-management", `{"modelId":"` + chatModel + `","action":"cancel"}`, http.StatusConflict},
		{"an import", "/_localbase/model-imports", `{"action":"cancel","job":"no-such-job"}`, http.StatusNotFound},
	}
	for _, mode := range []struct {
		name   string
		access *auth.BrowserAccess
		host   string
	}{{"no browser access", nil, ""}, {"direct browser access", directAccess(), "lb.example"}} {
		t.Run(mode.name, func(t *testing.T) {
			g := newMatrixGateway(t, mode.access)
			lib, err := library.Open(g.srv.Root, g.store, g.srv.Catalog, nil)
			if err != nil {
				t.Fatal(err)
			}
			g.srv.Library = lib
			key := func(scopes ...string) string {
				t.Helper()
				_, secret, err := g.store.CreateAPIKey(context.Background(), strings.Join(scopes, "+"), scopes)
				if err != nil {
					t.Fatal(err)
				}
				return secret
			}
			reader, chat, manager := key("models:read"), key("inference:chat"), key("models:manage")
			for _, c := range cancels {
				send := func(credential string) rawResponse {
					t.Helper()
					h := map[string]string{"Content-Type": "application/json"}
					if credential != "" {
						h["Authorization"] = "Bearer " + credential
					}
					return g.raw(t, http.MethodPost, c.path, mode.host, h, c.body)
				}
				if r := send(""); r.status != 401 {
					t.Errorf("%s with no credential: %d %.200q", c.name, r.status, r.body)
				}
				for who, credential := range map[string]string{"no permissions": g.noScopes, "models:read": reader, "inference:chat": chat} {
					if r := send(credential); r.status != 403 {
						t.Errorf("%s with %s: %d %.200q", c.name, who, r.status, r.body)
					}
				}
				if r := send(manager); r.status != c.allowed {
					t.Errorf("%s with models:manage: %d %.200q, want %d", c.name, r.status, r.body, c.allowed)
				}
			}
			if s := g.llm.Snapshot().State; s != "idle" {
				t.Errorf("a cancel changed the LLM runtime: %q", s)
			}
		})
	}
}

// Whatever a path looks like before it is cleaned, a request with no credential
// reaches nothing it could not reach by its clean name.
func TestPathTricksReachNoProtectedHandler(t *testing.T) {
	g := newMatrixGateway(t, nil)
	targets := []string{
		"//v1/models", "/v1//models", "/v1/./models", "/v1/../v1/models", "/./v1/models", "/app/../v1/models", "/health/../v1/models",
		"/health/../_localbase/api-keys", "/app/assets/../../v1/models", "/app/assets/..%2f..%2fv1/models",
		"/%2e%2e/v1/models", "/v1/%2e%2e/v1/models", "/v1%2fmodels", "/v1/models%2f..%2f..%2f_localbase%2fapi-keys",
		"/%76%31/models", "/v1/models/", "/v1/models.", "/v1/models/.", "/v1/models/..", "/v1/models%00", "/v1/models%20",
		"/_localbase/api-keys/", "/_localbase/api-keys.", "/_localbase/api-keys%2f", "/_LOCALBASE/api-keys", "/V1/MODELS", "/v1/Models",
		"/v1/m%CE%BFdels", "/v1%EF%BC%8Fmodels", "/_localbase%EF%BC%8Fapi-keys",
		"/app/api/../../v1/models", "/app/api/..%2f..%2fv1/models", "/app/api//_localbase/api-keys", "/app/api/_localbase/api-keys/",
		"/app/api/_localbase%2fapi-keys", "/app/api/_localbase/voices/..%2f..%2fapi-keys", "/app/api/_localbase/voices/..%2Faudio",
		"/app/api/_localbase/runs/../events", "/app/api/v1/videos/../../_localbase/api-keys/content", "/app/session/../api/_localbase/api-keys",
		"http://internal/v1/models", "http://internal/_localbase/api-keys",
	}
	for _, target := range targets {
		for _, method := range matrixMethods {
			g.bodyRead.Store(0)
			r := g.send(t, method, target, "", nil)
			if n := g.bodyRead.Load(); n != 0 {
				t.Errorf("%s %s: %d request body bytes were read", method, target, n)
			}
			switch r.status {
			case 301, 302, 307, 308:
				// A redirect is only ever to this gateway, and it does not authenticate anything.
				if strings.Contains(r.location, "://") || strings.HasPrefix(r.location, "//") {
					t.Errorf("%s %s: redirected to %q", method, target, r.location)
				}
			case 400, 401, 403, 404, 405:
			default:
				t.Errorf("%s %s: %d %.200q", method, target, r.status, r.body)
			}
		}
	}
	g.checkNoSideEffects(t)
}

// A connection upgrade is not a way around authentication.
func TestUpgradeHeadersChangeNothing(t *testing.T) {
	g := newMatrixGateway(t, nil)
	upgrades := []map[string]string{
		{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": base64.StdEncoding.EncodeToString([]byte("the sample nonce"))},
		{"Connection": "Upgrade, HTTP2-Settings", "Upgrade": "h2c", "HTTP2-Settings": "AAMAAABkAAQAAP__"},
	}
	for _, path := range []string{"/v1/models", "/_localbase/api-keys", "/_localbase/models", "/app/api/_localbase/api-keys", "/_localbase/runs"} {
		for _, h := range upgrades {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				if r := g.send(t, method, path, "", h); r.status != 401 && r.status != 405 {
					t.Errorf("%s %s with %s: %d %.200q", method, path, h["Upgrade"], r.status, r.body)
				}
			}
		}
	}
}

// A conditional or ranged request is authenticated like any other.
func TestConditionalRequestsChangeNothing(t *testing.T) {
	g := newMatrixGateway(t, nil)
	cond := []map[string]string{{"Range": "bytes=0-0"}, {"If-None-Match": "*"}, {"If-Modified-Since": "Mon, 02 Jan 2006 15:04:05 GMT"},
		{"If-Range": "x", "Range": "bytes=0-0"}, {"If-Match": "*"}, {"If-Unmodified-Since": "Mon, 02 Jan 2006 15:04:05 GMT"}}
	for _, path := range []string{"/v1/models", "/_localbase/api-keys", "/_localbase/models", "/v1/videos/00000000-0000-0000-0000-000000000000/content",
		"/_localbase/voices/voice-000000000000/audio", "/app/lab/voices/preview", "/app/lab/results/x", "/app/voices/x/audio"} {
		for _, h := range cond {
			r := g.send(t, http.MethodGet, path, "", h)
			if r.status != 401 {
				t.Errorf("GET %s with %v: %d %.200q, want 401", path, h, r.status, r.body)
			}
		}
	}
}

// tokenExceptions are the routes the instance token opens beyond tokenRoutes, which
// answer nothing else. A route that starts honouring the token fails until it is here.
var tokenExceptions = map[string]string{
	"POST /_localbase/usage":            "the command line changes usage settings through a running gateway",
	"POST /_localbase/update":           "the command line changes update settings through a running gateway",
	"POST /_localbase/model-management": "the command line cancels an install; every other action is refused below",
}

// With the instance token, a request gets what a request without it gets, on every route
// and method, apart from the routes that exist for it and tokenExceptions.
func TestInstanceTokenRouteMatrix(t *testing.T) {
	g := newMatrixGateway(t, nil)
	release, err := g.srv.Root.Lock("127.0.0.1", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	withToken := map[string]string{"X-Localbase-Instance-Token": g.srv.Root.Instance().InstanceToken}
	seen := map[string]bool{}
	for path := range matrixPaths(gatewayPatterns(t)) {
		if slices.Contains(tokenRoutes, path) {
			continue
		}
		for _, method := range matrixMethods {
			without := g.send(t, method, path, "", nil)
			with := g.send(t, method, path, "", withToken)
			if with.status == without.status {
				continue
			}
			if _, ok := tokenExceptions[method+" "+path]; !ok {
				t.Errorf("%s %s: %d with the instance token, %d without: add it to tokenExceptions if that is meant", method, path, with.status, without.status)
			}
			seen[method+" "+path] = true
		}
	}
	for route := range tokenExceptions {
		if !seen[route] {
			t.Errorf("%s no longer answers the token differently: remove it from tokenExceptions", route)
		}
	}

	// On the one route that takes a token for one action: cancel only, and only with the token.
	const path = "/_localbase/model-management"
	post := func(action string, headers map[string]string) rawResponse {
		t.Helper()
		h := map[string]string{"Content-Type": "application/json"}
		for k, v := range headers {
			h[k] = v
		}
		return g.raw(t, http.MethodPost, path, "", h, `{"modelId":"`+chatModel+`","action":"`+action+`"}`)
	}
	// Nothing is running, so a cancel that got through is a 409.
	if r := post("cancel", withToken); r.status != 409 {
		t.Errorf("cancel with the token: %d %.200q, want 409", r.status, r.body)
	}
	if r := post("cancel", nil); r.status != 401 {
		t.Errorf("cancel without it: %d %.200q, want 401", r.status, r.body)
	}
	for _, action := range []string{"install", "uninstall", "enable", "disable", "activate"} {
		if r := post(action, withToken); r.status != 403 {
			t.Errorf("%s with the token: %d %.200q, want 403", action, r.status, r.body)
		}
		if r := post(action, nil); r.status != 401 {
			t.Errorf("%s without it: %d %.200q, want 401", action, r.status, r.body)
		}
	}
	g.checkNoSideEffects(t)
}
