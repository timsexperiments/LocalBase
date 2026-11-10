package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/gatewaytest"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/runs"
	"github.com/timsexperiments/LocalBase/go/internal/hub"
	"github.com/timsexperiments/LocalBase/go/internal/placement"
	"github.com/timsexperiments/LocalBase/go/internal/placement/placementtest"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/internal/version"
	"github.com/timsexperiments/LocalBase/go/internal/voices"
)

// The drift tests of DESIGN §30.2: a route without documentation, documentation
// without a route, a wrong method, permission, status or response field fails here.

// openAPIExempt lists what the document leaves out on purpose, by registered pattern.
var openAPIExempt = map[string]string{
	"/":                           "the catch-all that answers unknown paths",
	"/app/login":                  "browser sign-in page",
	auth.LocalOncePath:            "browser sign-in page",
	"/oidc/callback":              "browser sign-in callback",
	"/github/callback":            "browser sign-in callback",
	"/app/login/magic":            "browser sign-in page",
	"/magic-link/callback":        "browser sign-in callback",
	"/app/logout":                 "browser sign-out",
	"/app/session":                "the browser's session probe",
	"/app/api":                    "the browser's forwarding layer, same routes as documented",
	"/app/api/":                   "the browser's forwarding layer, same routes as documented",
	"/_localbase/instance":        "behind the instance token, for this machine's CLI",
	"/_localbase/default-model":   "behind the instance token, for this machine's CLI",
	"/_localbase/placement-state": "behind the instance token, for this machine's CLI",
	"/app":                        "the browser UI",
	"/app/":                       "the browser UI",
	"/{$}":                        "the browser UI",
}

// openAPIUIOnly are exempt patterns registered only when a UI is mounted.
var openAPIUIOnly = []string{"/app", "/app/", "/{$}"}

// openAPINoMethodCheck are documented paths whose handler never checked the
// method (TS parity, DESIGN §13.6 lists the JSON routes; these stayed).
var openAPINoMethodCheck = map[string]bool{
	"/v1/audio/transcriptions": true, // "any other method fails as an invalid form"
	"/v1/audio/translations":   true,
}

// openAPIProbeBody gets past validation to the permission check, for routes that
// validate first. The default body is "{}".
var openAPIProbeBody = map[string]string{
	"post /_localbase/access-management": `{"action":"remove-user","email":"nobody@example.test"}`,
}

// oaOp is one documented operation.
type oaOp struct {
	path, method string
	op           map[string]any
	item         map[string]any
}

type oaDoc struct{ root map[string]any }

func loadOpenAPI(t *testing.T) *oaDoc {
	t.Helper()
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(openAPISource))
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("openapi.json does not parse: %v", err)
	}
	return &oaDoc{root}
}

var oaMethods = []string{"get", "head", "post", "put", "patch", "delete"}

func (d *oaDoc) paths() []string {
	return slices.Sorted(maps.Keys(d.root["paths"].(map[string]any)))
}

func (d *oaDoc) operations() []oaOp {
	var out []oaOp
	for _, p := range d.paths() {
		item := d.root["paths"].(map[string]any)[p].(map[string]any)
		for _, m := range oaMethods {
			if op, ok := item[m].(map[string]any); ok {
				out = append(out, oaOp{p, m, op, item})
			}
		}
	}
	return out
}

// resolve follows a local $ref.
func (d *oaDoc) resolve(ref string) (map[string]any, bool) {
	rest, ok := strings.CutPrefix(ref, "#/")
	if !ok {
		return nil, false
	}
	var cur any = d.root
	for _, seg := range strings.Split(rest, "/") {
		seg = strings.NewReplacer("~1", "/", "~0", "~").Replace(seg)
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	m, ok := cur.(map[string]any)
	return m, ok
}

func (d *oaDoc) deref(m map[string]any) map[string]any {
	for {
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		next, ok := d.resolve(ref)
		if !ok {
			return m
		}
		m = next
	}
}

var oaParamRe = regexp.MustCompile(`\{([^}]+)\}`)

// concrete fills a documented path's parameters with a value the routes accept
// as a shape, without naming anything that exists.
func concrete(path string) string { return concreteWith(path, "no-such-model", "run_missing") }

// concreteWith fills them with a given model and run. Some handlers look the id up
// before anything else, so only a real one reaches the method and permission checks.
func concreteWith(path, model, run string) string {
	return oaParamRe.ReplaceAllStringFunc(path, func(m string) string {
		switch strings.Trim(m, "{}") {
		case "voice_id":
			return "voice-0123456789ab"
		case "run_id":
			return run
		case "job_id":
			return "00000000-0000-0000-0000-000000000000"
		}
		return model
	})
}

// --- the document on its own ------------------------------------------------------

func TestOpenAPIDocument(t *testing.T) {
	d := loadOpenAPI(t)
	if v, _ := d.root["openapi"].(string); !strings.HasPrefix(v, "3.1.") {
		t.Errorf("openapi = %q, want 3.1.x", v)
	}
	info, _ := d.root["info"].(map[string]any)
	if info == nil || info["title"] == nil {
		t.Fatal("info.title is required")
	}
	if info["version"] != openAPIVersionMark {
		t.Errorf("info.version = %v, want the mark serving replaces", info["version"])
	}
	// Nothing about one machine belongs in a public document.
	for _, s := range []string{"127.0.0.1", "/Users/", "localhost", "timsexperiments.foo"} {
		if bytes.Contains(openAPISource, []byte(s)) {
			t.Errorf("the document holds %q: it is public and the same everywhere", s)
		}
	}
	// Generic copy names no model.
	cat, err := catalog.LoadWithFormer()
	if err != nil {
		t.Fatal(err)
	}
	lower := bytes.ToLower(openAPISource)
	for _, spec := range cat.All() {
		for _, name := range []string{spec.ModelID, spec.Family} {
			if name != "" && bytes.Contains(lower, []byte(strings.ToLower(name))) {
				t.Errorf("the document names %q: copy names no model", name)
			}
		}
	}
	tags := map[string]bool{}
	for _, tag := range d.root["tags"].([]any) {
		tags[tag.(map[string]any)["name"].(string)] = true
	}
	ids := map[string]string{}
	for _, o := range d.operations() {
		where := o.method + " " + o.path
		id, _ := o.op["operationId"].(string)
		if id == "" {
			t.Errorf("%s: no operationId", where)
		} else if prev, dup := ids[id]; dup {
			t.Errorf("%s: operationId %s also on %s", where, id, prev)
		}
		ids[id] = where
		list, _ := o.op["tags"].([]any)
		if len(list) == 0 {
			t.Errorf("%s: no tag", where)
		}
		for _, tag := range list {
			if !tags[tag.(string)] {
				t.Errorf("%s: tag %v is not declared", where, tag)
			}
		}
		if s, _ := o.op["summary"].(string); s == "" {
			t.Errorf("%s: no summary", where)
		}
		responses, _ := o.op["responses"].(map[string]any)
		if len(responses) == 0 {
			t.Errorf("%s: no responses", where)
		}
		for code, r := range responses {
			if m := d.deref(r.(map[string]any)); m["description"] == nil {
				t.Errorf("%s: response %s has no description", where, code)
			}
		}
		// The permission is one the gateway knows, "any", or absent on a public route.
		perm, hasPerm := o.op["x-localbase-permission"].(string)
		sec, hasSec := o.op["security"].([]any)
		public := hasSec && len(sec) == 0
		switch {
		case public && hasPerm:
			t.Errorf("%s: public, yet names a permission", where)
		case !public && !hasPerm:
			t.Errorf("%s: no x-localbase-permission and not marked public (security: [])", where)
		case hasPerm && perm != "any" && !slices.Contains(auth.Permissions, perm):
			t.Errorf("%s: unknown permission %q", where, perm)
		}
		for _, need := range anyStrings(o.op["x-localbase-also-requires"]) {
			if !slices.Contains(auth.Permissions, need) {
				t.Errorf("%s: x-localbase-also-requires names unknown permission %q", where, need)
			}
		}
		for _, alt := range anyStrings(o.op["x-localbase-or-permission"]) {
			if !slices.Contains(auth.Permissions, alt) {
				t.Errorf("%s: x-localbase-or-permission names unknown permission %q", where, alt)
			}
		}
		want := "unstable"
		if strings.HasPrefix(o.path, "/v1/") || !strings.HasPrefix(o.path, "/_localbase/") {
			want = "stable"
		}
		if got, _ := o.op["x-localbase-stability"].(string); got != want {
			t.Errorf("%s: x-localbase-stability = %q, want %q (docs/versioning.md)", where, got, want)
		}
		// Path parameters declared exactly as the path names them.
		declared := map[string]bool{}
		for _, src := range []any{o.item["parameters"], o.op["parameters"]} {
			list, _ := src.([]any)
			for _, p := range list {
				pm := d.deref(p.(map[string]any))
				if pm["in"] == "path" {
					declared[pm["name"].(string)] = true
					if pm["required"] != true {
						t.Errorf("%s: path parameter %v must be required", where, pm["name"])
					}
				}
			}
		}
		named := map[string]bool{}
		for _, m := range oaParamRe.FindAllStringSubmatch(o.path, -1) {
			named[m[1]] = true
			if !declared[m[1]] {
				t.Errorf("%s: path parameter %s is not declared", where, m[1])
			}
		}
		for p := range declared {
			if !named[p] {
				t.Errorf("%s: declares path parameter %s the path does not have", where, p)
			}
		}
	}
	// Every $ref anywhere resolves, and the validator understands every keyword used.
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				if _, ok := d.resolve(ref); !ok {
					t.Errorf("%s: $ref %s does not resolve", at, ref)
				}
			}
			for k, e := range x {
				walk(e, at+"/"+k)
			}
		case []any:
			for i, e := range x {
				walk(e, at+"/"+strconv.Itoa(i))
			}
		}
	}
	walk(d.root, "#")
	for name, s := range d.root["components"].(map[string]any)["schemas"].(map[string]any) {
		checkKeywords(t, s, "#/components/schemas/"+name)
	}
	for _, o := range d.operations() {
		visitSchemas(d, o.op, func(s any, at string) { checkKeywords(t, s, o.method+" "+o.path+at) })
	}
}

// visitSchemas calls fn on each media-type schema of an operation.
func visitSchemas(d *oaDoc, op map[string]any, fn func(any, string)) {
	visit := func(content any, at string) {
		for mt, media := range asMap(content) {
			if s, ok := asMap(media)["schema"]; ok {
				fn(s, at+"/"+mt)
			}
		}
	}
	if rb := asMap(op["requestBody"]); rb != nil {
		visit(d.deref(rb)["content"], "/requestBody")
	}
	for code, r := range asMap(op["responses"]) {
		visit(d.deref(asMap(r))["content"], "/responses/"+code)
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// --- routes -------------------------------------------------------------------------

func TestOpenAPICoversEveryRoute(t *testing.T) {
	ts := openAPIGateway(t)
	srv := runServer(ts)
	d := loadOpenAPI(t)
	reached := map[string]bool{}
	for _, p := range d.paths() {
		req := httptest.NewRequest("GET", concrete(p), nil)
		_, pattern := srv.mux.Handler(req)
		if pattern == "" || pattern == "/" {
			t.Errorf("documented path %s reaches no route", p)
			continue
		}
		reached[pattern] = true
	}
	for _, pattern := range srv.routes {
		if _, exempt := openAPIExempt[pattern]; !exempt && !reached[pattern] {
			t.Errorf("route %s is registered but not in openapi.json: document it, or exempt it with a reason (DESIGN §30.1)", pattern)
		}
	}
	for pattern := range openAPIExempt {
		if !slices.Contains(srv.routes, pattern) && !slices.Contains(openAPIUIOnly, pattern) {
			t.Errorf("exempt route %s is no longer registered: remove the exemption", pattern)
		}
	}
	for _, pattern := range openAPIExempt {
		if pattern == "" {
			t.Error("an exemption needs a reason")
		}
	}
}

// goFuncs parses the sources of the gateway and every package under it, and returns each function
// by name. A name declared more than once maps to nil, so goFunc refuses it instead of reading
// whichever came last.
func goFuncs(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	out := map[string]*ast.FuncDecl{}
	fset := token.NewFileSet()
	for _, name := range testutil.GoSources(t, ".", gatewayPackages) {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if _, dup := out[fn.Name.Name]; dup {
					out[fn.Name.Name] = nil
					continue
				}
				out[fn.Name.Name] = fn
			}
		}
	}
	return out
}

// goFunc is the one function in fns called name. It fails the test if there is none, or more than
// one, since then what is read may not be the handler meant.
func goFunc(t *testing.T, fns map[string]*ast.FuncDecl, name string) *ast.FuncDecl {
	t.Helper()
	fn, ok := fns[name]
	switch {
	case !ok:
		t.Fatalf("no function called %s in the gateway's source", name)
	case fn == nil:
		t.Fatalf("more than one function is called %s in the gateway's source", name)
	}
	return fn
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// The wildcard routes hide several operations each. Their handlers are the
// source of truth: read what they branch on, and compare with the document.
func TestOpenAPISubroutes(t *testing.T) {
	d := loadOpenAPI(t)
	fns := goFuncs(t)
	doc := d.paths()
	under := func(prefix string) []string {
		var out []string
		for _, p := range doc {
			if rest, ok := strings.CutPrefix(p, prefix); ok && rest != "" {
				out = append(out, rest)
			}
		}
		sort.Strings(out)
		return out
	}

	// /_localbase/models/{id}/<suffix>: what modelMetadata cuts off.
	var modelSuffixes []string
	ast.Inspect(goFunc(t, fns, "ModelMetadata"), func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "CutSuffix" && len(call.Args) == 2 {
				if s, ok := stringLit(call.Args[1]); ok {
					modelSuffixes = append(modelSuffixes, s)
				}
			}
		}
		return true
	})
	sort.Strings(modelSuffixes)
	if got := under("/_localbase/models/{model_id}"); !slices.Equal(got, modelSuffixes) {
		t.Errorf("models/{id} suffixes: document %v, code %v", got, modelSuffixes)
	}

	// /_localbase/runs/{id}/<action>: the cases Route switches on.
	var actions []string
	ast.Inspect(goFunc(t, fns, "Route"), func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		if tag, ok := sw.Tag.(*ast.Ident); !ok || tag.Name != "action" {
			return true
		}
		for _, c := range sw.Body.List {
			for _, e := range c.(*ast.CaseClause).List {
				if s, ok := stringLit(e); ok && s != "" {
					actions = append(actions, "/"+s)
				}
			}
		}
		return false
	})
	sort.Strings(actions)
	if len(actions) == 0 {
		t.Error("found no actions in Route: the test no longer reads the code")
	}
	if got := under("/_localbase/runs/{run_id}"); !slices.Equal(got, actions) {
		t.Errorf("runs/{id} actions: document %v, code %v", got, actions)
	}

	// /_localbase/runs/v1/…: exactly Targets.
	targets := slices.Sorted(maps.Keys(runs.Targets))
	var documented []string
	for _, rest := range under("/_localbase/runs/v1/") {
		documented = append(documented, "/v1/"+rest)
	}
	if !slices.Equal(documented, targets) {
		t.Errorf("run targets: document %v, Targets %v", documented, targets)
	}

	// /_localbase/voices/{id}/<segment>: what voicesAPI compares parts[1] with.
	var voiceSegments []string
	ast.Inspect(goFunc(t, fns, "voicesAPI"), func(n ast.Node) bool {
		if b, ok := n.(*ast.BinaryExpr); ok && b.Op == token.EQL {
			if ix, ok := b.X.(*ast.IndexExpr); ok {
				if id, ok := ix.X.(*ast.Ident); ok && id.Name == "parts" {
					if s, ok := stringLit(b.Y); ok {
						voiceSegments = append(voiceSegments, "/"+s)
					}
				}
			}
		}
		return true
	})
	if got := under("/_localbase/voices/{voice_id}"); !slices.Equal(got, voiceSegments) {
		t.Errorf("voices/{id} segments: document %v, code %v", got, voiceSegments)
	}

	// Whatever the browser may call by exact path is documented: the route table's routes with ui
	// methods, the list allowedUIRoute reads.
	var browser []string
	for _, rt := range (&Server{}).routeTable() {
		if len(rt.ui) > 0 {
			browser = append(browser, rt.pattern)
		}
	}
	if len(browser) == 0 {
		t.Error("found no ui routes in the route table: the test no longer reads the code")
	}
	for _, p := range browser {
		if !slices.Contains(doc, p) {
			t.Errorf("the browser may call %s, but it is not documented", p)
		}
	}
}

// --- methods and permissions ---------------------------------------------------------

type oaClient struct {
	t    *testing.T
	base string
	key  string
}

func (c oaClient) do(method, path string, headers ...string) (int, string, []byte, http.Header) {
	c.t.Helper()
	return c.doBody("{}", method, path, headers...)
}

func (c oaClient) doBody(reqBody, method, path string, headers ...string) (int, string, []byte, http.Header) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(reqBody))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		Error struct {
			Code any `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(body, &env)
	code, _ := env.Error.Code.(string)
	return resp.StatusCode, code, body, resp.Header
}

func TestOpenAPIMethods(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	_, key, err := s.Store.CreateAPIKey(t.Context(), "all", auth.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	c := oaClient{t, ts.URL, key}
	d := loadOpenAPI(t)
	// A finished run of this key's, since a run's routes look the run up first. Each
	// probe gets its own: DELETE removes it.
	finishedRun := func() string {
		t.Helper()
		status, _, body, _ := c.doBody(gatewaytest.ChatBody("hi"), "POST", "/_localbase/runs/v1/chat/completions")
		var run struct {
			ID string `json:"id"`
		}
		if status != 202 || json.Unmarshal(body, &run) != nil || run.ID == "" {
			t.Fatalf("create run: %d %s", status, body)
		}
		if !gatewaytest.WaitFor(openAPIWait, func() bool {
			_, _, b, _ := c.do("GET", "/_localbase/runs/"+run.ID)
			return bytes.Contains(b, []byte(`"completed"`))
		}) {
			t.Fatal("the run did not complete")
		}
		return run.ID
	}
	for _, p := range d.paths() {
		item := d.root["paths"].(map[string]any)[p].(map[string]any)
		for _, m := range oaMethods {
			method := strings.ToUpper(m)
			run := "run_missing"
			if strings.Contains(p, "{run_id}") {
				run = finishedRun()
			}
			status, code, _, _ := c.do(method, concreteWith(p, gatewaytest.ChatModel, run))
			if run != "run_missing" {
				c.do("DELETE", "/_localbase/runs/"+run) // at most 32 are retained per owner
			}
			_, documented := item[m]
			switch {
			case documented && (status == 405 || code == "route_disabled"):
				t.Errorf("%s %s is documented, but answers %d %s", method, p, status, code)
			case !documented && status != 405 && !openAPINoMethodCheck[p]:
				t.Errorf("%s %s is not documented, but answers %d %s: document it or refuse it", method, p, status, code)
			}
		}
	}
}

func TestOpenAPIPermissions(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	ctx := t.Context()
	keys := map[string]string{}
	keyWith := func(scopes []string) string {
		name := strings.Join(scopes, ",")
		if k, ok := keys[name]; ok {
			return k
		}
		_, k, err := s.Store.CreateAPIKey(ctx, "k"+strconv.Itoa(len(keys)), scopes)
		if err != nil {
			t.Fatal(err)
		}
		keys[name] = k
		return k
	}
	d := loadOpenAPI(t)
	for _, o := range d.operations() {
		method, where := strings.ToUpper(o.method), o.method+" "+o.path
		path := concreteWith(o.path, gatewaytest.ChatModel, "run_missing")
		perm, hasPerm := o.op["x-localbase-permission"].(string)
		body, probed := openAPIProbeBody[where]
		if !probed {
			body = "{}"
		}
		anon, _, _, _ := oaClient{t, ts.URL, ""}.doBody(body, method, path)
		if !hasPerm { // public
			if anon == 401 || anon == 403 {
				t.Errorf("%s is documented public, but answers %d without a key", where, anon)
			}
			continue
		}
		if anon != 401 {
			t.Errorf("%s: no key answers %d, want 401", where, anon)
		}
		if perm == "any" {
			continue
		}
		alternatives := anyStrings(o.op["x-localbase-or-permission"])
		others := slices.DeleteFunc(slices.Clone(auth.Permissions), func(p string) bool { return p == perm || slices.Contains(alternatives, p) })
		if got, _, out, _ := (oaClient{t, ts.URL, keyWith(others)}).doBody(body, method, path); got != 403 {
			t.Errorf("%s: every permission but %s answers %d, want 403: %s", where, perm, got, firstBytes(out))
		}
		also := anyStrings(o.op["x-localbase-also-requires"])
		enough := append([]string{perm}, also...)
		if got, _, out, _ := (oaClient{t, ts.URL, keyWith(enough)}).doBody(body, method, path); got == 401 || got == 403 {
			t.Errorf("%s: %v answers %d: %s", where, enough, got, firstBytes(out))
		}
		// Each documented alternative is enough by itself.
		for _, alt := range alternatives {
			if got, _, out, _ := (oaClient{t, ts.URL, keyWith([]string{alt})}).doBody(body, method, path); got == 401 || got == 403 {
				t.Errorf("%s: %s is documented as enough, but answers %d: %s", where, alt, got, firstBytes(out))
			}
		}
		// What the document says it also needs, it does need. If that stops being true,
		// the route got simpler and the document should say so.
		for _, need := range also {
			fewer := slices.DeleteFunc(slices.Clone(enough), func(p string) bool { return p == need })
			if got, _, out, _ := (oaClient{t, ts.URL, keyWith(fewer)}).doBody(body, method, path); got != 403 {
				t.Errorf("%s: without %s answers %d, want 403: %s", where, need, got, firstBytes(out))
			}
		}
	}
}

func firstBytes(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}

// The document says a key cannot be used from a web page. Keep that true.
func TestOpenAPIKeysRefusedFromPages(t *testing.T) {
	ts := openAPIGateway(t)
	s := runServer(ts)
	_, key, err := s.Store.CreateAPIKey(t.Context(), "k", auth.DefaultKeyScopes)
	if err != nil {
		t.Fatal(err)
	}
	c := oaClient{t, ts.URL, key}
	if status, _, _, _ := c.do("GET", "/v1/models"); status != 200 {
		t.Fatalf("the key itself answers %d", status)
	}
	for _, h := range [][2]string{{"Origin", "https://example.test"}, {"Sec-Fetch-Site", "cross-site"}} {
		if status, _, _, hdr := c.do("GET", "/v1/models", h[0], h[1]); status != 401 {
			t.Errorf("with %s: %d, want 401", h[0], status)
		} else if hdr.Get("Access-Control-Allow-Origin") != "" {
			t.Error("the gateway sends no CORS headers")
		}
	}
	// X-Api-Key is the other documented scheme.
	if status, _, _, _ := (oaClient{t, ts.URL, ""}).do("GET", "/v1/models", "X-Api-Key", key); status != 200 {
		t.Errorf("X-Api-Key answers %d", status)
	}
}

// --- responses ------------------------------------------------------------------------

// matchPath finds the documented path a concrete path is an instance of: an exact
// path before a template, as the routes themselves do.
func (d *oaDoc) matchPath(path string) string {
	parts := strings.Split(path, "/")
	match := func(p string) bool {
		segs := strings.Split(p, "/")
		if len(segs) != len(parts) {
			return false
		}
		for i, seg := range segs {
			if !strings.HasPrefix(seg, "{") && seg != parts[i] {
				return false
			}
		}
		return true
	}
	template := ""
	for _, p := range d.paths() {
		if !match(p) {
			continue
		}
		if !strings.Contains(p, "{") {
			return p
		}
		template = p
	}
	return template
}

// checkResponse compares a real answer with the operation's documented one.
func (d *oaDoc) checkResponse(t *testing.T, method, path string, status int, hdr http.Header, body []byte) {
	t.Helper()
	where := method + " " + path
	// A code the scan of the source can't see, such as one that is built up, is still caught here.
	var answer struct {
		Error struct {
			Code any `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &answer) == nil {
		if code, ok := answer.Error.Code.(string); ok && code != "" && !slices.Contains(errorCodeEnum(t), code) {
			t.Errorf("%s answered with the code %s, which isn't on the list. %s", where, code, errorCodeFix)
		}
	}
	if q := strings.IndexByte(path, '?'); q >= 0 {
		path = path[:q]
	}
	doc := d.matchPath(path)
	if doc == "" {
		t.Errorf("%s: no documented path", where)
		return
	}
	op := asMap(asMap(d.root["paths"].(map[string]any)[doc])[strings.ToLower(method)])
	if op == nil {
		t.Errorf("%s: method not documented on %s", where, doc)
		return
	}
	resp := asMap(asMap(op["responses"])[strconv.Itoa(status)])
	if resp == nil {
		t.Errorf("%s: status %d is not documented: %s", where, status, firstBytes(body))
		return
	}
	resp = d.deref(resp)
	content := asMap(resp["content"])
	if content == nil {
		if len(body) > 0 && method != "HEAD" {
			t.Errorf("%s: %d documents no body, got %s", where, status, firstBytes(body))
		}
		return
	}
	ct := hdr.Get("Content-Type")
	mt, _, _ := strings.Cut(ct, ";")
	media := asMap(content[strings.TrimSpace(mt)])
	if media == nil {
		t.Errorf("%s: %d answered %s, not documented", where, status, ct)
		return
	}
	schema, ok := media["schema"]
	if !ok || !strings.Contains(mt, "json") {
		return
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Errorf("%s: not JSON: %v", where, err)
		return
	}
	for _, problem := range d.validate(schema, v, "$") {
		t.Errorf("%s (%d): %s", where, status, problem)
	}
}

// The real answers, against the document.
func TestOpenAPIResponses(t *testing.T) {
	// Nothing here reaches the network: the Hub is the recorded responses.
	t.Setenv("HF_ENDPOINT", openAPIHub(t).URL)
	t.Setenv("HF_TOKEN", "")
	ts := openAPIGateway(t)
	s := runServer(ts)
	ctx := t.Context()
	self, key, err := s.Store.CreateAPIKey(ctx, "all", auth.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	_, reader, _ := s.Store.CreateAPIKey(ctx, "reader", []string{"models:read"})
	d := loadOpenAPI(t)
	send := func(key, method, path, ctype string, body []byte) (int, http.Header, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		if len(body) > 1<<20 {
			// A refused body is drained only so far (DESIGN 31), then the connection closes
			// and a client still sending can see a broken pipe instead of the answer.
			req.Header.Set("Expect", "100-continue")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, out
	}
	check := func(method, path, body string, want ...int) []byte {
		t.Helper()
		status, hdr, out := send(key, method, path, "application/json", []byte(body))
		if !slices.Contains(want, status) {
			t.Errorf("%s %s: %d, want %v: %s", method, path, status, want, firstBytes(out))
		}
		d.checkResponse(t, method, path, status, hdr, out)
		return out
	}
	form := func(path string, build func(*multipart.Writer), want ...int) []byte {
		t.Helper()
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		build(w)
		w.Close()
		status, hdr, out := send(key, "POST", path, w.FormDataContentType(), b.Bytes())
		if !slices.Contains(want, status) {
			t.Errorf("POST %s: %d, want %v: %s", path, status, want, firstBytes(out))
		}
		d.checkResponse(t, "POST", path, status, hdr, out)
		return out
	}
	// as asks with another key, or none, and compares the answer all the same.
	as := func(who, method, path, body string, want int) []byte {
		t.Helper()
		status, hdr, out := send(who, method, path, "application/json", []byte(body))
		if status != want {
			t.Errorf("%s %s: %d, want %d: %s", method, path, status, want, firstBytes(out))
		}
		d.checkResponse(t, method, path, status, hdr, out)
		return out
	}
	idOf := func(field string, body []byte) string {
		t.Helper()
		var m map[string]any
		json.Unmarshal(body, &m)
		id, _ := m[field].(string)
		if id == "" {
			t.Fatalf("no %s in %s", field, firstBytes(body))
		}
		return id
	}
	finished := func(id string) {
		t.Helper()
		if !gatewaytest.WaitFor(openAPIWait, func() bool {
			_, _, b := send(key, "GET", "/_localbase/runs/"+id, "", nil)
			var r struct {
				EndedAt *string `json:"endedAt"`
			}
			return json.Unmarshal(b, &r) == nil && r.EndedAt != nil
		}) {
			t.Fatalf("run %s did not finish", id)
		}
	}
	oversized := `{"x":"` + strings.Repeat("x", httpx.MaxManagementBody) + `"}`

	// Public.
	check("GET", "/health", "", 200)
	check("GET", "/health/ready", "", 200)
	check("HEAD", "/health", "", 200)
	check("HEAD", "/health/ready", "", 200)

	// The errors every client meets.
	{
		status, hdr, out := send("", "GET", "/v1/models", "", nil)
		if status != 401 {
			t.Errorf("no key: %d", status)
		}
		d.checkResponse(t, "GET", "/v1/models", status, hdr, out)
		status, hdr, out = send(reader, "POST", "/v1/chat/completions", "application/json", []byte(gatewaytest.ChatBody("hi")))
		if status != 403 {
			t.Errorf("reader chat: %d", status)
		}
		d.checkResponse(t, "POST", "/v1/chat/completions", status, hdr, out)
	}

	// /v1 models.
	check("GET", "/v1/models", "", 200)
	check("GET", "/v1/models/"+gatewaytest.ChatModel, "", 200)
	check("GET", "/v1/models/no-such-model", "", 404)

	// Chat, embeddings, decisions.
	check("POST", "/v1/chat/completions", gatewaytest.ChatBody("hi"), 200)
	check("POST", "/v1/chat/completions", `{"model":"`+gatewaytest.ChatModel+`","messages":[{"role":"user","content":"hi"}],"stream":true}`, 200)
	check("POST", "/v1/chat/completions", `{"model":"`+gatewaytest.ChatModel+`","messages":[]}`, 400)
	check("POST", "/v1/chat/completions", `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`, 404)
	check("POST", "/v1/embeddings", `{"model":"`+embeddingModel+`","input":"hi"}`, 200)
	check("POST", "/v1/embeddings", `{"model":""}`, 400)
	check("POST", "/v1/systemone", `{"model":"`+decisionModel+`","state":"s","questions":{"q":{"type":"noul"}}}`, 200)
	check("POST", "/v1/systemone", `{"model":"`+gatewaytest.ChatModel+`","state":"s","questions":{"q":{"type":"noul"}}}`, 400)

	// Audio and images: the fake runtimes answer as the real ones were measured.
	if openAPIHasMedia {
		check("POST", "/v1/audio/speech", `{"model":"`+speechModel+`","input":"hello","voice":"default"}`, 200)
		check("POST", "/v1/audio/speech", `{"model":"`+speechModel+`"}`, 400)
		form("/v1/audio/transcriptions", func(w *multipart.Writer) {
			part, _ := w.CreateFormFile("file", "a.wav")
			part.Write(minimalWAV())
			w.WriteField("model", openAPISTTModel)
			w.WriteField("response_format", "verbose_json")
		}, 200)
		form("/v1/audio/transcriptions", func(w *multipart.Writer) { w.WriteField("model", openAPISTTModel) }, 400)
		check("POST", "/v1/images/generations", `{"model":"`+openAPIImageModel+`","prompt":"a red apple","size":"256x256","response_format":"b64_json"}`, 200)
		check("POST", "/v1/images/generations", `{"prompt":1}`, 400)
	}

	// Video jobs: the routes, without a job running.
	check("GET", "/v1/videos/00000000-0000-0000-0000-000000000000", "", 404)
	check("POST", "/v1/videos", `{}`, 400)

	// Models and the host.
	check("GET", "/_localbase/models", "", 200)
	check("GET", "/_localbase/models?kind=llm", "", 200)
	check("GET", "/_localbase/models/"+gatewaytest.ChatModel, "", 200)
	check("GET", "/_localbase/models/no-such-model", "", 404)
	check("GET", "/_localbase/models/"+gatewaytest.ChatModel+"/admission", "", 200)
	check("GET", "/_localbase/models/"+gatewaytest.ChatModel+"/admission?assumeInstalled=1", "", 200)
	check("POST", "/_localbase/models/"+gatewaytest.ChatModel+"/unload", "", 200, 404)
	check("GET", "/_localbase/host", "", 200)
	check("PUT", "/_localbase/computer-name", `{"name":"Test"}`, 200)
	check("PUT", "/_localbase/computer-name", `{"name":""}`, 200)
	check("PUT", "/_localbase/computer-name", `{}`, 400)
	check("PUT", "/_localbase/computer-name", `{"name":"`+strings.Repeat("x", 65)+`"}`, 400)
	check("GET", "/_localbase/default-chat-models", "", 200)
	check("POST", "/_localbase/default-chat-models", `{"models":["`+gatewaytest.ChatModel+`"]}`, 200)
	check("POST", "/_localbase/default-chat-models", `{"models":["`+gatewaytest.ChatModel+`","`+gatewaytest.ChatModel+`"]}`, 400)
	check("POST", "/_localbase/default-chat-models", `{"models":["no-such-model"]}`, 400)
	check("POST", "/_localbase/default-chat-models", `{"models":[]}`, 400)
	check("GET", "/_localbase/model-management", "", 200)
	check("POST", "/_localbase/model-management", `{"modelId":"no-such-model","action":"install"}`, 400)
	check("POST", "/_localbase/model-management", `{"modelId":"`+gatewaytest.ChatModel+`"}`, 400)
	check("POST", "/_localbase/model-management", `{"modelId":"`+gatewaytest.ChatModel+`","action":"install"}`, 409)
	check("POST", "/_localbase/model-management", `{"modelId":"`+gatewaytest.ChatModel+`","action":"uninstall"}`, 409)
	check("POST", "/_localbase/model-management", `{"modelId":"`+gatewaytest.ChatModel+`","action":"disable"}`, 409)
	check("POST", "/_localbase/model-management", `{"modelId":"`+gatewaytest.ChatModel+`","action":"enable"}`, 200)
	check("POST", "/_localbase/model-management", `{"modelId":"`+gatewaytest.ChatModel+`","action":"activate"}`, 200)
	check("POST", "/_localbase/model-management", oversized, 413)
	check("GET", "/_localbase/model-management", "", 200) // now with an operation on the model

	// Every catalog entry, read and previewed: all of it starts nothing.
	var catalogued struct {
		Data []struct {
			ID      string `json:"id"`
			Catalog struct {
				Kind string `json:"kind"`
			} `json:"catalog"`
		} `json:"data"`
	}
	json.Unmarshal(check("GET", "/_localbase/models", "", 200), &catalogued)
	if len(catalogued.Data) == 0 {
		t.Error("the catalog listed nothing")
	}
	for _, m := range catalogued.Data {
		check("GET", "/_localbase/models/"+m.ID, "", 200)
		check("GET", "/_localbase/model-backend?model="+m.ID, "", 200)
		want := []int{200}
		if m.Catalog.Kind != "llm" {
			want = []int{200, 404} // this gateway's media runtimes each know one model
		}
		check("GET", "/_localbase/models/"+m.ID+"/admission", "", want...)
		check("GET", "/_localbase/models/"+m.ID+"/admission?assumeInstalled=1", "", want...)
	}
	check("GET", "/_localbase/models/no-such-model/admission", "", 404)
	if openAPIHasMedia {
		image := s.Image
		s.Image = nil
		check("GET", "/_localbase/models/"+openAPIImageModel+"/admission", "", 501)
		s.Image = image
	}
	check("GET", "/_localbase/models?kind=stt,tts&installed=false&sort=memory&reverse=true&fit=fits&license=open&capability=x&q=x", "", 200)

	// Placement: the stored choice, the next load's split, what is refused.
	placed := "/_localbase/models/" + gatewaytest.ChatModel + "/placement"
	check("GET", placed, "", 200)
	check("GET", "/_localbase/models/no-such-model/placement", "", 404)
	check("PUT", "/_localbase/models/no-such-model/placement", `{"mode":"cpu"}`, 404)
	check("PUT", placed, `{"mode":"cpu","contextSize":16384}`, 200)
	check("PUT", placed, `{"mode":"auto","gpuLayers":3}`, 400)
	check("PUT", placed, `{"mode":"custom","cpuMoeLayers":1}`, 400)
	check("PUT", placed, `{"mode":"auto","contextSize":10}`, 400)
	check("PUT", placed, `{"mode":"nonsense"}`, 400)
	check("PUT", placed, `{`, 400)
	check("DELETE", placed, "", 200)
	check("DELETE", "/_localbase/models/no-such-model/placement", "", 404)
	expert := 1
	if err := s.Store.SetPlacement(ctx, gatewaytest.ChatModel, openAPIDevice, store.Placement{Mode: placement.Custom, CPUMoELayers: &expert}); err != nil {
		t.Fatal(err)
	}
	if out := check("GET", placed, "", 200); !bytes.Contains(out, []byte(`"nextError"`)) {
		t.Errorf("a stored choice this device refuses: %s", firstBytes(out))
	}
	check("DELETE", placed, "", 200)
	// A discrete GPU with nothing free: the split in its own words, and what cannot fit.
	base, _, _ := placementGateway(t, placement.Discrete, 0)
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", placed, "", 200},
		{"PUT", placed, `{"mode":"gpu"}`, 409},
		{"PUT", placed, `{"mode":"custom","cpuMoeLayers":9}`, 400},
		{"GET", "/_localbase/models/" + gatewaytest.ChatModel, "", 200},
		{"GET", "/_localbase/models", "", 200},
	} {
		req, _ := http.NewRequest(c.method, base+c.path, strings.NewReader(c.body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("discrete %s %s: %d, want %d: %s", c.method, c.path, resp.StatusCode, c.want, firstBytes(out))
		}
		d.checkResponse(t, c.method, c.path, resp.StatusCode, resp.Header, out)
	}

	// A loaded model: its runtime and split, then every answer an unload has.
	unloading := "/_localbase/models/" + gatewaytest.ChatModel + "/unload"
	check("POST", "/v1/chat/completions", gatewaytest.ChatBody("hi"), 200)
	check("GET", "/_localbase/models/"+gatewaytest.ChatModel, "", 200)
	check("GET", placed, "", 200)
	check("PUT", placed, `{"mode":"gpu"}`, 200) // pending for the next load
	check("DELETE", placed, "", 200)
	check("GET", "/_localbase/host", "", 200)
	busy := idOf("id", check("POST", "/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("fake:slow 200"), 202))
	if !gatewaytest.WaitFor(openAPIWait, func() bool {
		_, _, b := send(key, "GET", "/_localbase/runs/"+busy, "", nil)
		return bytes.Contains(b, []byte(`"in_progress"`))
	}) {
		t.Fatal("the run did not start")
	}
	check("GET", "/_localbase/host", "", 200)
	check("GET", "/_localbase/runs/"+busy, "", 200)
	check("POST", unloading+"?wait=false", "", 409)
	if out := check("POST", "/_localbase/runs/"+busy+"/cancel", "", 200); !bytes.Contains(out, []byte(`"stopped"`)) {
		t.Errorf("a stopped run: %s", firstBytes(out))
	}
	check("GET", "/_localbase/runs/"+busy+"/events", "", 200)
	check("POST", unloading+"?wait=maybe", "", 400)
	check("POST", unloading, "", 200)
	check("POST", unloading, "", 404)
	check("POST", "/_localbase/models/no-such-model/unload", "", 404)

	// Backends: a body that is not JSON answers in the management shape.
	check("GET", "/_localbase/backends", "", 200)
	check("GET", "/_localbase/model-backend?model="+gatewaytest.ChatModel, "", 200)
	check("GET", "/_localbase/model-backend?model=no-such-model", "", 404)
	check("GET", "/_localbase/auth-profiles", "", 200)
	profiles := filepath.Join(s.Root.Dir, "credentials", "auth-profiles.json")
	if err := os.MkdirAll(filepath.Dir(profiles), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profiles, []byte(`{"profiles":[{"id":"doc","origins":["https://files.example.test"],"headers":{"X-Doc":"secret-value"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := check("GET", "/_localbase/auth-profiles", "", 200); bytes.Contains(out, []byte("secret-value")) {
		t.Errorf("a header value left the file: %s", firstBytes(out))
	}
	check("POST", "/_localbase/model-backend", `{`, 400)
	check("POST", "/_localbase/model-backend", `{}`, 400)
	check("POST", "/_localbase/model-backend", `{"action":"set","model":"`+gatewaytest.ChatModel+`"}`, 400)
	check("POST", "/_localbase/model-backend", `{"action":"set","model":"`+gatewaytest.ChatModel+`","backendId":"no-such-backend"}`, 400)
	check("POST", "/_localbase/model-backend", `{"action":"reset","model":"no-such-model"}`, 404)
	check("POST", "/_localbase/model-backend", `{"action":"reset","model":"`+gatewaytest.ChatModel+`"}`, 200)
	check("POST", "/_localbase/model-backend", oversized, 413)

	// Imports: a local file needs no network.
	check("GET", "/_localbase/model-imports", "", 404)
	check("POST", "/_localbase/model-imports", `{`, 400)
	check("POST", "/_localbase/model-imports", `{}`, 400)
	check("POST", "/_localbase/model-imports", oversized, 413)
	check("POST", "/_localbase/model-imports", `{"action":"resolve","source":"x","task":"nonsense"}`, 400)
	check("POST", "/_localbase/model-imports", `{"action":"resolve","source":"x","backend":"no-such-backend"}`, 400)
	check("POST", "/_localbase/model-imports", `{"action":"resolve","source":"no-such-file"}`, 400)
	weights := filepath.Join(scratchDir(t), "doc-weights-q4_k_m.gguf")
	placementtest.WriteMoEGGUF(t, weights, 1<<20)
	importing := `"source":` + strconv.Quote(weights)
	check("POST", "/_localbase/model-imports", `{"action":"resolve",`+importing+`,"task":"chat"}`, 200)
	check("POST", "/_localbase/model-imports", `{"action":"start",`+importing+`}`, 400)
	job := idOf("job", check("POST", "/_localbase/model-imports", `{"action":"start",`+importing+`,"task":"chat","id":"doc-import","name":"Doc"}`, 202))
	check("GET", "/_localbase/model-imports?id="+job, "", 200)
	if !gatewaytest.WaitFor(openAPIWait, func() bool {
		_, _, b := send(key, "GET", "/_localbase/model-imports?id="+job, "", nil)
		return !bytes.Contains(b, []byte(`"state":"running"`))
	}) {
		t.Fatal("the import did not finish")
	}
	if out := check("GET", "/_localbase/model-imports?id="+job, "", 200); !bytes.Contains(out, []byte(`"state":"done"`)) {
		t.Errorf("the import: %s", firstBytes(out))
	}
	check("GET", "/_localbase/model-backend?model=doc-import", "", 200)
	check("GET", "/_localbase/models/doc-import", "", 404) // the catalog route knows no imports

	// Discovery, against the recorded Hub.
	const repo = "bartowski/Qwen_Qwen3.5-0.8B-GGUF"
	check("POST", "/_localbase/model-discovery", `{}`, 400)
	check("POST", "/_localbase/model-discovery", `{"action":"search","kind":"video"}`, 400)
	check("POST", "/_localbase/model-discovery", `{"action":"preview","repo":"ab"}`, 400)
	check("POST", "/_localbase/model-discovery", `{"action":"preview","repo":"`+repo+`","extra":1}`, 400)
	check("POST", "/_localbase/model-discovery", `{"x":"`+strings.Repeat("x", 2048)+`"}`, 413)
	check("POST", "/_localbase/model-discovery", `{"action":"install-recipe","modelId":"no-such-model"}`, 404)
	check("POST", "/_localbase/model-discovery", `{"action":"install-recipe","modelId":"`+gatewaytest.ChatModel+`"}`, 400)
	check("POST", "/_localbase/model-discovery", `{"action":"search","query":"qwen coder"}`, 200)
	check("POST", "/_localbase/model-discovery", `{"action":"preview","repo":"`+repo+`"}`, 200)
	check("POST", "/_localbase/model-discovery", `{"action":"preview","repo":"nobody/does-not-exist-404"}`, 400)
	check("POST", "/_localbase/model-discovery", `{"action":"preview","repo":"not-a-repo"}`, 400)
	check("POST", "/_localbase/model-discovery", `{"action":"preview","repo":"nobody/hub-down"}`, 502)

	// The Hub's token: the fake Hub refuses one and knows the others. The token never comes back.
	check("GET", "/_localbase/hf-token", "", 200)
	check("POST", "/_localbase/hf-token", `{`, 400)
	check("POST", "/_localbase/hf-token", `{}`, 400)
	check("POST", "/_localbase/hf-token", `{"action":"save","token":"has a space"}`, 400)
	check("POST", "/_localbase/hf-token", `{"action":"clear","token":"accepted-token"}`, 400)
	check("POST", "/_localbase/hf-token", `{"action":"validate"}`, 409)
	check("POST", "/_localbase/hf-token", `{"action":"save","token":"rejected-token"}`, 400)
	check("POST", "/_localbase/hf-token", `{"action":"save","token":"accepted-token"}`, 200)
	check("POST", "/_localbase/hf-token", `{"action":"validate"}`, 200)
	check("POST", "/_localbase/hf-token", `{"action":"save","token":"rejected-token","validate":false}`, 200)
	check("POST", "/_localbase/hf-token", `{"action":"validate"}`, 200)
	check("GET", "/_localbase/hf-token", "", 200)
	check("POST", "/_localbase/hf-token", `{"action":"clear"}`, 200)
	check("POST", "/_localbase/hf-token", `{"x":"`+strings.Repeat("x", 1024)+`"}`, 413)

	// Keys: the shape, the secret once.
	created := check("POST", "/_localbase/api-keys", `{"action":"create","name":"doc","scopes":["models:read"]}`, 201)
	var made struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
		Secret string `json:"secret"`
	}
	if json.Unmarshal(created, &made) != nil || made.Secret == "" {
		t.Fatalf("create key: %s", created)
	}
	check("GET", "/_localbase/api-keys", "", 200)
	check("POST", "/_localbase/api-keys", `{"action":"rotate","keyId":"`+made.Key.ID+`"}`, 200)
	check("POST", "/_localbase/api-keys", `{"action":"set-scopes","keyId":"`+made.Key.ID+`","scopes":["models:read"]}`, 200)
	check("POST", "/_localbase/api-keys", `{"action":"revoke","keyId":"`+made.Key.ID+`"}`, 200)
	check("POST", "/_localbase/api-keys", `{"action":"revoke","keyId":"nope"}`, 404)
	check("POST", "/_localbase/api-keys", `{}`, 400)
	check("POST", "/_localbase/api-keys", `{`, 400)
	check("POST", "/_localbase/api-keys", `{"action":"create","name":"x","scopes":["nonsense"]}`, 400)
	check("POST", "/_localbase/api-keys", `{"action":"revoke","keyId":"`+self.ID+`"}`, 409)
	check("POST", "/_localbase/api-keys", oversized, 413)
	dated := check("POST", "/_localbase/api-keys", `{"action":"create","name":"dated","scopes":[],"expiresDays":1}`, 201)
	if !bytes.Contains(dated, []byte(`"expiresAt"`)) {
		t.Errorf("a dated key: %s", firstBytes(dated))
	}
	check("GET", "/_localbase/api-keys", "", 200) // revoked and dated keys among them
	as("", "GET", "/_localbase/api-keys", "", 401)
	as(reader, "GET", "/_localbase/api-keys", "", 403)
	as("", "POST", "/_localbase/api-keys", `{}`, 401)
	as(reader, "POST", "/_localbase/api-keys", `{}`, 403)

	// Access and sessions.
	check("GET", "/_localbase/access-management", "", 200)
	check("POST", "/_localbase/access-management", `{}`, 400)
	check("POST", "/_localbase/access-management", `{`, 400)
	check("POST", "/_localbase/access-management", oversized, 413)
	as("", "GET", "/_localbase/access-management", "", 401)
	as(reader, "GET", "/_localbase/access-management", "", 403)
	as("", "POST", "/_localbase/access-management", `{}`, 401)
	as(reader, "POST", "/_localbase/access-management", `{}`, 403)
	const (
		origin  = `"origin":"https://gateway.example.test","permissions":["models:read"]`
		who     = `"identity":{"issuer":"https://id.example.test","subject":"one","verifiedEmail":"a@example.test"}`
		invite  = `{"action":"invite-user","email":"a@example.test","roles":["admin"]}`
		theUser = `"email":"a@example.test"`
		policy  = `{"roles":[{"name":"admin","description":"All of it.","permissions":["access:manage"]},{"name":"viewer","description":"","permissions":[]}],` +
			`"bindings":[{"kind":"email","role":"admin","email":"owner@example.test"},{"kind":"email-domain","role":"viewer","domain":"example.test"},` +
			`{"kind":"subject","role":"viewer","issuer":"https://id.example.test","subject":"two"}],"defaultRole":"viewer"}`
	)
	check("POST", "/_localbase/access-management", `{"action":"test-policy",`+who+`}`, 409)
	check("POST", "/_localbase/access-management", invite, 409)
	check("POST", "/_localbase/access-management", `{"action":"apply-policy","policy":`+policy+`,"expectedPolicyRevision":null}`, 409)
	check("POST", "/_localbase/access-management", `{"action":"test-email-delivery","to":"a@example.test"}`, 409)
	check("POST", "/_localbase/access-management", `{"action":"upsert-magic-link","registration":{"kind":"magic-link","id":"mail","name":"Email"},`+origin+`}`, 409)
	check("POST", "/_localbase/access-management", `{"action":"configure-email-delivery","config":{"host":"mail.example.test","port":587,"security":"starttls","authentication":{"kind":"password","username":"u","password":"p"},"from":"from@example.test"}}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"upsert-magic-link","registration":{"kind":"magic-link","id":"mail","name":"Email"},`+origin+`}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"upsert-oidc","registration":{"kind":"oidc","id":"sso","name":"Sign-in","issuer":"https://id.example.test","clientId":"c","clientAuthentication":{"kind":"client-secret-basic","clientSecret":"s"}},`+origin+`}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"upsert-github","registration":{"kind":"github-oauth","id":"gh","name":"GitHub","clientId":"c","clientSecret":"s"},`+origin+`}`, 200)
	check("GET", "/_localbase/access-management", "", 200) // every kind of registration
	check("POST", "/_localbase/access-management", `{"action":"disable-email-delivery"}`, 409)
	check("POST", "/_localbase/access-management", `{"action":"remove-registration","registrationId":"nope"}`, 404)
	check("POST", "/_localbase/access-management", `{"action":"remove-registration","registrationId":"gh"}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"remove-registration","registrationId":"mail"}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"disable-email-delivery"}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"configure-email-delivery","config":{"host":"127.0.0.1","port":`+strconv.Itoa(freePort(t))+`,"security":"starttls","authentication":{"kind":"none"},"from":"from@example.test"}}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"test-email-delivery","to":"a@example.test"}`, 502)
	check("POST", "/_localbase/access-management", `{"action":"disable-email-delivery"}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"test-policy",`+who+`}`, 200) // no policy yet
	check("POST", "/_localbase/access-management", invite, 409)                             // no policy yet
	applied := check("POST", "/_localbase/access-management", `{"action":"apply-policy","policy":`+policy+`,"expectedPolicyRevision":null}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"apply-policy","policy":`+policy+`,"expectedPolicyRevision":null}`, 409)
	rev := idOf("policyRevision", applied)
	check("GET", "/_localbase/access-management", "", 200) // the policy, its roles
	check("POST", "/_localbase/access-management", `{"action":"invite-user","email":"a@example.test","roles":["nope"]}`, 400)
	check("POST", "/_localbase/access-management", invite, 201)
	check("POST", "/_localbase/access-management", invite, 409)
	check("POST", "/_localbase/access-management", `{"action":"replace-user-roles",`+theUser+`,"roles":["viewer"]}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"disable-user",`+theUser+`}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"enable-user",`+theUser+`}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"enable-user","email":"nobody@example.test"}`, 404)
	check("POST", "/_localbase/access-management", `{"action":"test-policy",`+who+`}`, 200)
	check("GET", "/_localbase/access-management", "", 200) // a user
	check("POST", "/_localbase/access-management", `{"action":"clear-policy","expectedPolicyRevision":"`+rev+`"}`, 409)
	check("POST", "/_localbase/access-management", `{"action":"remove-user",`+theUser+`}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"clear-policy","expectedPolicyRevision":"`+strings.Repeat("0", 64)+`"}`, 409)
	check("POST", "/_localbase/access-management", `{"action":"clear-policy","expectedPolicyRevision":"`+rev+`"}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"remove-registration","registrationId":"sso"}`, 200) // none left: disabled
	check("POST", "/_localbase/access-management", `{"action":"configure-cloudflare","provider":{"kind":"cloudflare-access","teamDomain":"team.cloudflareaccess.com","audience":"a"},`+origin+`}`, 200)
	check("GET", "/_localbase/access-management", "", 200)
	check("POST", "/_localbase/access-management", `{"action":"disable"}`, 200)
	check("POST", "/_localbase/access-management", `{"action":"disable"}`, 200) // already off
	if _, _, err := auth.CreateLocalSession(ctx, s.Store); err != nil {
		t.Fatal(err)
	}
	check("GET", "/_localbase/local-sessions", "", 200)
	check("POST", "/_localbase/local-sessions", `{"action":"revoke","id":"no-such-session"}`, 200)
	check("POST", "/_localbase/local-sessions", `{}`, 400)
	check("POST", "/_localbase/local-sessions", `{"action":"revoke"}`, 400)
	check("POST", "/_localbase/local-sessions", `{"action":"revoke-all","id":"one"}`, 400)
	check("POST", "/_localbase/local-sessions", `{"action":"revoke-all"}`, 200)
	check("GET", "/_localbase/local-sessions", "", 200)

	// Voices: the whole life of one.
	// Usage: what the gateway answers, with requests counted above.
	check("GET", "/_localbase/usage", "", 200)
	check("GET", "/_localbase/usage?range=30d&scope=all&group=principal&group=model&group=route", "", 200)
	check("GET", "/_localbase/usage?range=1y", "", 400)
	check("POST", "/_localbase/usage", `{"action":"set-retention","days":30}`, 200)
	check("POST", "/_localbase/usage", `{"action":"set-recording","enabled":true}`, 200)
	check("POST", "/_localbase/usage", `{"action":"set-retention","days":0}`, 400)
	check("POST", "/_localbase/usage", `{}`, 400)

	check("GET", "/_localbase/voices", "", 200)
	check("GET", "/_localbase/voices/voice-0123456789ab", "", 404)
	check("GET", "/_localbase/voices/voice-0123456789ab/audio", "", 404)
	check("PATCH", "/_localbase/voices/voice-0123456789ab", `{"name":"x"}`, 404)
	check("DELETE", "/_localbase/voices/voice-0123456789ab", "", 404)
	voice := func(name, consent string, audio []byte, want int) []byte {
		t.Helper()
		return form("/_localbase/voices", func(w *multipart.Writer) {
			w.WriteField("name", name)
			w.WriteField("consent", consent)
			part, _ := w.CreateFormFile("audio", "a.wav")
			part.Write(audio)
		}, want)
	}
	clip := voiceClipWAV()
	voice("Doc voice", "false", clip, 400)
	voice("default", "true", clip, 400)
	voice("Doc voice", "true", minimalWAV(), 400)
	voice("Doc voice", "true", []byte("not a recording"), 400)
	form("/_localbase/voices", func(w *multipart.Writer) { w.WriteField("name", "Doc voice") }, 400)
	check("POST", "/_localbase/voices", `{"name":"Doc voice"}`, 415)
	voice("Doc voice", "true", make([]byte, voices.MaxUploadBytes+voiceMultipartOverhead), 413)
	madeVoice := idOf("id", voice("Doc voice", "true", clip, 201))
	voice("Doc voice", "true", clip, 409)
	voicePath := "/_localbase/voices/" + madeVoice
	otherVoice := idOf("id", voice("Other voice", "true", clip, 201))
	check("PATCH", voicePath, `{"name":"Other voice"}`, 409)
	check("DELETE", "/_localbase/voices/"+otherVoice, "", 200)
	check("GET", "/_localbase/voices", "", 200)
	check("GET", voicePath, "", 200)
	as(reader, "GET", voicePath, "", 200) // without its creator
	as(reader, "GET", "/_localbase/voices", "", 200)
	as(reader, "PATCH", voicePath, `{"name":"x"}`, 403)
	as(reader, "DELETE", voicePath, "", 403)
	as(reader, "GET", voicePath+"/audio", "", 403)
	check("GET", voicePath+"/audio", "", 200)
	check("PATCH", voicePath, `{"name":"Renamed voice"}`, 200)
	check("PATCH", voicePath, `{"name":"Renamed voice","extra":1}`, 400)
	check("PATCH", voicePath, `{"name":"default"}`, 400)
	check("PATCH", voicePath, `{}`, 400)
	check("PATCH", voicePath, `{`, 400)
	check("PATCH", voicePath, `{"name":"`+strings.Repeat("x", 1024)+`"}`, 413)
	check("DELETE", voicePath, "", 200)
	check("GET", voicePath, "", 404)
	check("GET", voicePath+"/audio", "", 404)
	stored := s.Voices
	s.Voices = nil
	check("GET", "/_localbase/voices", "", 501)
	check("POST", "/_localbase/voices", `{}`, 501)
	check("GET", voicePath, "", 501)
	check("PATCH", voicePath, `{"name":"x"}`, 501)
	check("DELETE", voicePath, "", 501)
	check("GET", voicePath+"/audio", "", 501)
	s.Voices = stored

	// Runs: the whole life of one.
	createdRun := check("POST", "/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("hi"), 202)
	var run struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(createdRun, &run) != nil || run.ID == "" {
		t.Fatalf("create run: %s", createdRun)
	}
	check("GET", "/_localbase/runs", "", 200)
	if !gatewaytest.WaitFor(openAPIWait, func() bool {
		_, _, b := send(key, "GET", "/_localbase/runs/"+run.ID, "", nil)
		return bytes.Contains(b, []byte(`"completed"`))
	}) {
		t.Error("the run did not complete")
	}
	check("GET", "/_localbase/runs/"+run.ID, "", 200)
	check("GET", "/_localbase/runs/"+run.ID+"/events", "", 200)
	check("GET", "/_localbase/runs/"+run.ID+"/result", "", 409)
	check("POST", "/_localbase/runs/"+run.ID+"/cancel", "", 200)
	check("DELETE", "/_localbase/runs/"+run.ID, "", 200)
	check("GET", "/_localbase/runs/"+run.ID, "", 404)
	check("POST", "/_localbase/runs/run_missing/cancel", "", 404)
	check("GET", "/_localbase/runs/run_missing/result", "", 404)
	check("GET", "/_localbase/runs/run_missing/events", "", 404)
	check("DELETE", "/_localbase/runs/run_missing", "", 404)

	// What /v1 refuses, a run refuses the same way, and no run exists.
	check("POST", "/_localbase/runs/v1/chat/completions", `{"model":"`+gatewaytest.ChatModel+`","messages":[]}`, 400)
	check("POST", "/_localbase/runs/v1/chat/completions", `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`, 404)
	check("POST", "/_localbase/runs/v1/chat/completions", `{`, 400)
	check("POST", "/_localbase/runs/v1/systemone", `{"model":"`+gatewaytest.ChatModel+`","state":"s","questions":{"q":{"type":"noul"}}}`, 400)
	as(reader, "POST", "/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("hi"), 403)
	as("", "GET", "/_localbase/runs", "", 401)
	for target := range runs.Targets {
		check("POST", "/_localbase/runs"+target, `{}`, 400, 501)
	}
	check("POST", "/_localbase/runs/v1/chat/completions", `{"x":"`+strings.Repeat("x", 26<<20)+`"}`, 413)
	check("POST", "/_localbase/runs/v1/audio/speech", `{"x":"`+strings.Repeat("x", runs.MaxBody)+`"}`, 413)

	// A run that fails, one that is interrupted, and the limit on active runs.
	failing := idOf("id", check("POST", "/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("fake:context"), 202))
	finished(failing)
	if out := check("GET", "/_localbase/runs/"+failing, "", 200); !bytes.Contains(out, []byte(`"failed"`)) {
		t.Errorf("a failed run: %s", firstBytes(out))
	}
	check("GET", "/_localbase/runs/"+failing+"/result", "", 409)
	active := []string{}
	for range runs.ActivePerOwner {
		active = append(active, idOf("id", check("POST", "/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("fake:slow 200"), 202)))
	}
	check("GET", "/_localbase/runs", "", 200) // running and queued
	check("POST", "/_localbase/runs/v1/chat/completions", gatewaytest.ChatBody("hi"), 429)
	// An interrupted run: TestARunPastItsMaxDurationIsInterrupted.
	for _, id := range active {
		check("DELETE", "/_localbase/runs/"+id, "", 200)
	}

	// Runs of the other targets: each one's result, in its own type.
	decided := idOf("id", check("POST", "/_localbase/runs/v1/systemone", `{"model":"`+decisionModel+`","state":"s","questions":{"q":{"type":"noul"}}}`, 202))
	finished(decided)
	check("GET", "/_localbase/runs/"+decided, "", 200)
	check("GET", "/_localbase/runs/"+decided+"/result", "", 200)
	check("GET", "/_localbase/runs", "", 200)
	if openAPIHasMedia {
		spoken := idOf("id", check("POST", "/_localbase/runs/v1/audio/speech", `{"model":"`+speechModel+`","input":"hello","voice":"default"}`, 202))
		finished(spoken)
		check("GET", "/_localbase/runs/"+spoken, "", 200)
		check("GET", "/_localbase/runs/"+spoken+"/result", "", 200)
		check("GET", "/_localbase/runs/"+spoken+"/events?after=1", "", 200)
		drawn := idOf("id", check("POST", "/_localbase/runs/v1/images/generations", `{"model":"`+openAPIImageModel+`","prompt":"a red apple","size":"256x256","response_format":"b64_json"}`, 202))
		finished(drawn)
		check("GET", "/_localbase/runs/"+drawn+"/result", "", 200)
		for _, target := range []string{"transcriptions", "translations"} {
			heard := idOf("id", form("/_localbase/runs/v1/audio/"+target, func(w *multipart.Writer) {
				part, _ := w.CreateFormFile("file", "a.wav")
				part.Write(minimalWAV())
				w.WriteField("model", openAPISTTModel)
				w.WriteField("response_format", "text")
			}, 202))
			finished(heard)
			check("GET", "/_localbase/runs/"+heard, "", 200)
			check("GET", "/_localbase/runs/"+heard+"/result", "", 200)
		}
		form("/_localbase/runs/v1/images/edits", func(w *multipart.Writer) { w.WriteField("model", openAPIImageModel) }, 400)
	}

	// Last, because adding reloads the catalog this gateway's fixtures come from.
	for _, spec := range s.Catalog.All() {
		if spec.Origin == "built-in" {
			check("POST", "/_localbase/model-discovery", `{"action":"add","repo":"`+repo+`","modelId":"`+spec.ModelID+`"}`, 409)
			break
		}
	}
	check("POST", "/_localbase/model-discovery", `{"action":"add","repo":"`+repo+`","modelId":"doc-added"}`, 200)
	check("POST", "/_localbase/model-discovery", `{"action":"add","repo":"`+repo+`","modelId":"doc-added"}`, 200) // replaces it

	// The published feed in force: its notes and recipes. The recipe's model is
	// selected, so its install is refused and nothing is downloaded.
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "catalog", "feed-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	feed, err := catalog.ParseFeed(raw)
	if err != nil {
		t.Fatal(err)
	}
	annotations, _ := catalog.ParseAnnotations(feed.AnnotationItems)
	layered, err := catalog.LoadLayers("", &catalog.RemoteLayer{URL: "feed", Annotations: annotations})
	if err != nil {
		t.Fatal(err)
	}
	s.Catalog.Replace(layered)
	check("GET", "/_localbase/models", "", 200)
	check("POST", "/_localbase/model-discovery", `{"action":"search","query":"qwen coder"}`, 200)
	for _, repo := range []string{repo, "ggml-org/Clef-Flash-GGUF", "Qwen/Qwen3-Coder-Next-GGUF"} {
		check("POST", "/_localbase/model-discovery", `{"action":"preview","repo":"`+repo+`"}`, 200)
	}
	if out := check("POST", "/_localbase/model-discovery", `{"action":"install-recipe","modelId":"`+decisionModel+`"}`, 200); !bytes.Contains(out, []byte(`"failed"`)) {
		t.Errorf("a refused recipe install: %s", firstBytes(out))
	}

	// All of that, and another process still sees this one attached.
	newLockAsker(t, s.Root.DB()).mustBeRefused(t, "every route above")
}

// openAPIHub is the Hub, offline: the responses internal/hub records, and a token check.
func openAPIHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "hub-down") {
			w.WriteHeader(500)
			return
		}
		if r.URL.Path == "/api/whoami-v2" {
			if r.Header.Get("Authorization") == "Bearer rejected-token" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"name":"someone","auth":{"accessToken":{"role":"read"}}}`))
			return
		}
		sum := sha256.Sum256([]byte(hub.PublicEndpoint + r.URL.RequestURI()))
		b, err := os.ReadFile(filepath.Join("..", "hub", "testdata", "hub", hex.EncodeToString(sum[:])[:24]+".json"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var f struct {
			Status int    `json:"status"`
			Body   string `json:"body"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			t.Error(err)
		}
		w.WriteHeader(f.Status)
		w.Write([]byte(f.Body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAPIServed(t *testing.T) {
	ts := openAPIGateway(t) // no credential is needed, with authentication on
	c := oaClient{t, ts.URL, ""}
	status, _, body, hdr := c.do("GET", "/openapi.json")
	if status != 200 {
		t.Fatalf("GET: %d %s", status, firstBytes(body))
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := hdr.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q", cc)
	}
	etag := hdr.Get("ETag")
	if !strings.HasPrefix(etag, `"`) || len(etag) < 8 {
		t.Errorf("ETag = %q", etag)
	}
	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Info.Version != version.Version {
		t.Errorf("served version %q, want %q", doc.Info.Version, version.Version)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1.") {
		t.Errorf("openapi = %q", doc.OpenAPI)
	}
	if status, _, b, _ := c.do("GET", "/openapi.json", "If-None-Match", etag); status != 304 || len(b) != 0 {
		t.Errorf("If-None-Match: %d, %d bytes", status, len(b))
	}
	if status, _, b, h := c.do("HEAD", "/openapi.json"); status != 200 || len(b) != 0 || h.Get("ETag") != etag {
		t.Errorf("HEAD: %d, %d bytes, ETag %q", status, len(b), h.Get("ETag"))
	}
	if status, code, _, h := c.do("POST", "/openapi.json"); status != 405 || code != "method_not_allowed" || !strings.Contains(h.Get("Allow"), "GET") {
		t.Errorf("POST: %d %s, Allow %q", status, code, h.Get("Allow"))
	}
	// An unknown path still answers as it did, so nothing else became public.
	if status, _, _, _ := c.do("GET", "/openapi.yaml"); status != 401 {
		t.Errorf("an unknown path answers %d without a key, want 401", status)
	}
}

// --- a validator for the keywords the document uses --------------------------------------

// oaValidated are checked by validate; oaAnnotations carry no check.
var (
	oaValidated = []string{"$ref", "type", "properties", "required", "additionalProperties", "items", "enum", "const",
		"anyOf", "oneOf", "allOf", "minimum", "maximum", "exclusiveMinimum", "minItems", "maxItems", "minLength",
		"maxLength", "pattern", "minProperties"}
	oaAnnotations = []string{"title", "description", "default", "examples", "format", "deprecated", "readOnly", "writeOnly",
		"$comment", "contentMediaType", "contentEncoding"}
)

// checkKeywords fails on a keyword the validator would silently ignore.
func checkKeywords(t *testing.T, schema any, at string) {
	t.Helper()
	m := asMap(schema)
	if m == nil {
		return
	}
	for k, v := range m {
		switch {
		case slices.Contains(oaValidated, k), slices.Contains(oaAnnotations, k):
		default:
			t.Errorf("%s: keyword %q: the test's validator does not know it", at, k)
		}
		switch k {
		case "properties":
			for name, sub := range asMap(v) {
				checkKeywords(t, sub, at+"/properties/"+name)
			}
		case "items", "additionalProperties":
			checkKeywords(t, v, at+"/"+k)
		case "anyOf", "oneOf", "allOf":
			list, _ := v.([]any)
			for i, sub := range list {
				checkKeywords(t, sub, at+"/"+k+"/"+strconv.Itoa(i))
			}
		}
	}
}

// validate checks a value and returns what is wrong. It is stricter than JSON
// Schema in one way, on purpose: a field the schema does not name fails unless the
// schema says additionalProperties, so a response cannot grow undocumented fields.
func (d *oaDoc) validate(schema any, v any, at string) []string {
	if b, ok := schema.(bool); ok {
		if !b {
			return []string{at + ": not allowed"}
		}
		return nil
	}
	s := asMap(schema)
	if s == nil {
		return nil
	}
	if ref, ok := s["$ref"].(string); ok {
		target, ok := d.resolve(ref)
		if !ok {
			return []string{at + ": $ref " + ref + " does not resolve"}
		}
		return d.validate(target, v, at)
	}
	var out []string
	if t, ok := s["type"]; ok && !oaTypeMatches(t, v) {
		return []string{fmt.Sprintf("%s: %s, want type %v", at, firstJSON(v), t)}
	}
	if list, ok := s["enum"].([]any); ok && !slices.ContainsFunc(list, func(e any) bool { return oaEqual(e, v) }) {
		out = append(out, fmt.Sprintf("%s: %s is not in the enum %v", at, firstJSON(v), list))
	}
	if c, ok := s["const"]; ok && !oaEqual(c, v) {
		out = append(out, fmt.Sprintf("%s: %s, want %v", at, firstJSON(v), c))
	}
	if list, ok := s["allOf"].([]any); ok {
		for _, sub := range list {
			out = append(out, d.validate(sub, v, at)...)
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		list, ok := s[key].([]any)
		if !ok {
			continue
		}
		passed := 0
		var first []string
		for _, sub := range list {
			problems := d.validate(sub, v, at)
			if len(problems) == 0 {
				passed++
			} else if first == nil {
				first = problems
			}
		}
		if passed == 0 || (key == "oneOf" && passed > 1) {
			out = append(out, fmt.Sprintf("%s: %s matches %d of %d %s branches; first problem: %v", at, firstJSON(v), passed, len(list), key, first))
		}
	}
	switch x := v.(type) {
	case map[string]any:
		for _, r := range anyStrings(s["required"]) {
			if _, ok := x[r]; !ok {
				out = append(out, at+": required field "+r+" is missing")
			}
		}
		props := asMap(s["properties"])
		extra, hasExtra := s["additionalProperties"]
		for k, e := range x {
			if sub, ok := props[k]; ok {
				out = append(out, d.validate(sub, e, at+"."+k)...)
			} else if hasExtra {
				out = append(out, d.validate(extra, e, at+"."+k)...)
			} else if _, isObject := s["properties"]; isObject {
				out = append(out, at+": field "+k+" is not documented")
			}
		}
		if n, ok := s["minProperties"].(float64); ok && float64(len(x)) < n {
			out = append(out, at+": too few properties")
		}
	case []any:
		if items, ok := s["items"]; ok {
			for i, e := range x {
				out = append(out, d.validate(items, e, at+"["+strconv.Itoa(i)+"]")...)
			}
		}
		if n, ok := s["minItems"].(float64); ok && float64(len(x)) < n {
			out = append(out, at+": too few items")
		}
		if n, ok := s["maxItems"].(float64); ok && float64(len(x)) > n {
			out = append(out, at+": too many items")
		}
	case string:
		if n, ok := s["minLength"].(float64); ok && float64(len([]rune(x))) < n {
			out = append(out, at+": too short")
		}
		if n, ok := s["maxLength"].(float64); ok && float64(len([]rune(x))) > n {
			out = append(out, at+": too long")
		}
		if p, ok := s["pattern"].(string); ok {
			if re, err := regexp.Compile(p); err != nil || !re.MatchString(x) {
				out = append(out, fmt.Sprintf("%s: %q does not match %s", at, x, p))
			}
		}
	case float64:
		if n, ok := s["minimum"].(float64); ok && x < n {
			out = append(out, fmt.Sprintf("%s: %v below minimum %v", at, x, n))
		}
		if n, ok := s["maximum"].(float64); ok && x > n {
			out = append(out, fmt.Sprintf("%s: %v above maximum %v", at, x, n))
		}
		if n, ok := s["exclusiveMinimum"].(float64); ok && x <= n {
			out = append(out, fmt.Sprintf("%s: %v not above %v", at, x, n))
		}
	}
	return out
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func oaTypeMatches(t, v any) bool {
	if list, ok := t.([]any); ok {
		return slices.ContainsFunc(list, func(e any) bool { return oaTypeMatches(e, v) })
	}
	switch t {
	case "null":
		return v == nil
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	}
	return false
}

func oaEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func firstJSON(v any) string {
	b, _ := json.Marshal(v)
	return firstBytes(b)
}

// The validator itself: a check that never fails proves nothing.
func TestOpenAPIValidator(t *testing.T) {
	d := &oaDoc{map[string]any{"components": map[string]any{"schemas": map[string]any{"S": map[string]any{"type": "string"}}}}}
	parse := func(s string) any {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		name, schema, value string
		ok                  bool
	}{
		{"type", `{"type":"string"}`, `"a"`, true},
		{"wrong type", `{"type":"string"}`, `1`, false},
		{"type list", `{"type":["string","null"]}`, `null`, true},
		{"integer", `{"type":"integer"}`, `1.5`, false},
		{"required", `{"type":"object","required":["a"],"properties":{"a":{}}}`, `{}`, false},
		{"undocumented field", `{"type":"object","properties":{"a":{}}}`, `{"a":1,"b":2}`, false},
		{"additional allowed", `{"type":"object","properties":{"a":{}},"additionalProperties":true}`, `{"b":2}`, true},
		{"additional typed", `{"type":"object","additionalProperties":{"type":"string"}}`, `{"b":2}`, false},
		{"enum", `{"enum":["a"]}`, `"b"`, false},
		{"const", `{"const":"a"}`, `"a"`, true},
		{"ref", `{"$ref":"#/components/schemas/S"}`, `1`, false},
		{"missing ref", `{"$ref":"#/components/schemas/X"}`, `1`, false},
		{"anyOf", `{"anyOf":[{"type":"string"},{"type":"integer"}]}`, `true`, false},
		{"oneOf both", `{"oneOf":[{"type":"number"},{"type":"integer"}]}`, `1`, false},
		{"items", `{"type":"array","items":{"type":"string"}}`, `["a",1]`, false},
		{"minimum", `{"minimum":2}`, `1`, false},
		{"pattern", `{"pattern":"^a"}`, `"b"`, false},
	} {
		if problems := d.validate(parse(c.schema), parse(c.value), "$"); (len(problems) == 0) != c.ok {
			t.Errorf("%s: %v", c.name, problems)
		}
	}
}
