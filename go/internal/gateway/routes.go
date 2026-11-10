package gateway

import (
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/gateway/httpx"
)

// route is one path the gateway serves. The table is the one list: it builds the mux, the list
// openapi_test compares with the document, and what usage counts a request as.
type route struct {
	pattern string
	handler http.HandlerFunc
	// usage is what a request here is counted as in the hourly totals (DESIGN §32); "" when it
	// isn't counted.
	usage string
	// ui is what the browser may call this exact path with through /app/api (allowedUIRoute);
	// none when it may not.
	ui []string
	// uiSub says the same for the paths under a pattern that ends in a wildcard or "/", given what
	// follows the pattern's fixed part.
	uiSub func(rest, method string) bool
	// methods, when set, are the only methods the route takes: any other is answered 405 with them as
	// Allow, once the caller may use the route at all (see compose). Routes whose methods depend on
	// the path below them, or that answer something before their method check, check their own.
	methods []string
	// guarded routes check the caller first (guard), for permission; "" is any authenticated caller.
	guarded    bool
	permission string
	// audit names the area a change through this route is audited under; describe says what
	// changed, for routes that record more than the area (auditedRoute).
	audit    string
	describe changeDescriber
}

// compose wraps a route's handler in what the table says, in the order the routes had it: the
// method gate innermost, around the handler, the permission check around that, and the audit
// around that. A caller who may not use a route is refused before it learns the route's methods.
func (s *Server) compose(rt route) http.HandlerFunc {
	h := rt.handler
	if len(rt.methods) > 0 {
		h = methodGate(rt.methods, h)
	}
	if rt.guarded {
		h = s.guard(rt.permission, h)
	}
	switch {
	case rt.audit != "" && rt.describe != nil:
		h = s.auditedRoute(rt.audit, rt.describe, h)
	case rt.audit != "":
		h = s.audited(rt.audit, h)
	}
	return h
}

// routeTable is every path the gateway serves, in the order they are registered.
func (s *Server) routeTable() []route {
	routes := []route{
		{pattern: "/health", methods: []string{"GET", "HEAD"}, handler: s.health},
		{pattern: "/openapi.json", methods: []string{"GET", "HEAD"}, handler: s.openAPI},
		{pattern: "/app/login", handler: s.access.UIAccess},
		{pattern: auth.LocalOncePath, handler: s.access.UIAccess},
		{pattern: "/oidc/callback", handler: s.access.UIAccess},
		{pattern: "/github/callback", handler: s.access.UIAccess},
		{pattern: "/app/login/magic", handler: s.access.UIAccess},
		{pattern: "/magic-link/callback", handler: s.access.UIAccess},
		{pattern: "/app/logout", handler: s.access.UIAccess},
		{pattern: "/app/session", handler: s.access.UIAccess},
		{pattern: "/app/api", handler: s.access.UIAccess},
		{pattern: "/app/api/", handler: s.access.UIAccess},
		{pattern: "/health/ready", methods: []string{"GET", "HEAD"}, handler: s.ready},
		{pattern: "/_localbase/instance", handler: s.instance},
		{pattern: "/_localbase/host", methods: []string{"GET"}, guarded: true, permission: "models:read", handler: s.hostAPI, ui: []string{"GET"}},
		{pattern: "/_localbase/computer-name", audit: "computer-name", describe: computerNameChange, handler: s.computerNameAPI, ui: []string{"PUT"}},
		{pattern: "/_localbase/default-model", handler: s.defaultModel}, // for the local CLI: an empty 404 without its instance token, by design
		{pattern: "/_localbase/placement-state", handler: s.models.PlacementRuntimeState},
		{pattern: "/v1/models", methods: []string{"GET"}, guarded: true, permission: "models:read", handler: s.listModels},
		{pattern: "/v1/models/{model...}", guarded: true, permission: "models:read", handler: s.retrieveModel},
		{pattern: "/v1/chat/completions", methods: []string{"POST"}, guarded: true, permission: "inference:chat", handler: s.chat, usage: "chat", ui: []string{"POST"}},
		{pattern: "/v1/embeddings", methods: []string{"POST"}, guarded: true, permission: "inference:embeddings", handler: s.embeddings, usage: "embeddings", ui: []string{"POST"}},
		{pattern: "/v1/systemone", methods: []string{"POST"}, guarded: true, permission: "inference:decision", handler: s.systemone, usage: "decision", ui: []string{"POST"}},
		{pattern: "/v1/audio/transcriptions", methods: []string{"POST"}, guarded: true, permission: "inference:transcription", handler: s.transcription, usage: "transcription", ui: []string{"POST"}},
		{pattern: "/v1/audio/translations", methods: []string{"POST"}, guarded: true, permission: "inference:transcription", handler: s.transcription, usage: "translation"},
		{pattern: "/v1/audio/speech", methods: []string{"POST"}, guarded: true, permission: "inference:speech", handler: s.speech, usage: "speech", ui: []string{"POST"}},
		{pattern: "/v1/images/generations", methods: []string{"POST"}, guarded: true, permission: "inference:image", handler: s.imageGeneration, usage: "image", ui: []string{"POST"}},
		{pattern: "/v1/images/edits", methods: []string{"POST"}, guarded: true, permission: "inference:image", handler: s.imageEdit, usage: "image-edit", ui: []string{"POST"}},
		{pattern: "/v1/videos", methods: []string{"POST"}, guarded: true, permission: "inference:video", handler: s.videoCreate, usage: "video", ui: []string{"POST"}},
		{pattern: "/v1/videos/{job_id}", handler: s.videoJobRoute("status"), uiSub: uiVideoJob},
		{pattern: "/v1/videos/{job_id}/content", handler: s.videoJobRoute("content")},
		{pattern: "/v1/videos/{job_id}/cancel", handler: s.videoJobRoute("cancel")},
		{pattern: "/_localbase/runs", methods: []string{"GET"}, guarded: true, permission: "", handler: s.runs.List, ui: []string{"GET"}},
		{pattern: "/_localbase/runs/{rest...}", guarded: true, permission: "", handler: s.runs.Route, uiSub: uiRun},
		{pattern: "/_localbase/models", guarded: true, permission: "models:read", handler: s.models.ModelMetadata, ui: []string{"GET"}},
		{pattern: "/_localbase/models/{model_id...}", handler: s.auditedModelRoute(func(w http.ResponseWriter, r *http.Request) {
			s.guard(modelRoutePermission(r), s.models.ModelMetadata)(w, r)
		}), uiSub: uiModel},
		{pattern: "/_localbase/access-management", audit: "access", handler: s.keys.AccessManagement, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/api-keys", audit: "api-key", handler: s.keys.APIKeys, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/hf-token", audit: "hf-token", describe: hfTokenChange, handler: s.keys.HFToken, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/backends", methods: []string{"GET"}, handler: s.models.BackendsAPI, ui: []string{"GET"}},
		{pattern: "/_localbase/model-backend", methods: []string{"GET", "POST"}, audit: "model-backend", handler: s.models.ModelBackendAPI, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/model-management", audit: "models", describe: modelsChange, handler: s.models.ModelManagementAPI, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/default-chat-models", audit: "default-models", describe: defaultModelsChange, handler: s.models.DefaultChatModelsAPI},
		{pattern: "/_localbase/model-discovery", audit: "discovery", describe: discoveryChange, handler: s.models.ModelDiscoveryAPI, ui: []string{"POST"}},
		{pattern: "/_localbase/model-imports", methods: []string{"GET", "POST"}, audit: "imports", describe: importsChange, handler: s.models.ModelImportsAPI, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/auth-profiles", methods: []string{"GET"}, handler: s.models.AuthProfilesAPI, ui: []string{"GET"}},
		{pattern: "/_localbase/local-sessions", handler: s.access.LocalSessionsAPI, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/usage", handler: s.usageAPI, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/update", handler: s.updateAPI, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/voices", audit: "voices", describe: voicesChange, handler: s.voicesAPI, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/voices/", audit: "voices", describe: voicesChange, handler: s.voicesAPI, uiSub: uiVoice},
	}
	if s.UI != nil {
		routes = append(routes,
			route{pattern: "/app", handler: s.UI.ServeHTTP},
			route{pattern: "/app/", handler: s.UI.ServeHTTP},
			route{pattern: "/{$}", handler: s.UI.ServeHTTP},
		)
	}
	routes = append(routes, route{pattern: "/", handler: s.unmatched})
	for i := range routes {
		routes[i].handler = s.compose(routes[i])
	}
	return routes
}

// tableIndex is what is looked up in the route table by path, built once from it: usage labels and
// the browser's allow-list. Built on first use, not as a package variable: the table's handlers
// lead back to recordUsage and allowedUIRoute, which read it.
func tableIndex() *routeIndex {
	indexOnce.Do(func() {
		index = &routeIndex{usage: map[string]string{}, uiExact: map[string][]string{}}
		for _, rt := range (&Server{}).routeTable() { // only to read the table: no handler runs
			if rt.usage != "" {
				index.usage[rt.pattern] = rt.usage
			}
			if len(rt.ui) > 0 {
				index.uiExact[rt.pattern] = rt.ui
			}
			if rt.uiSub != nil {
				prefix, _, _ := strings.Cut(rt.pattern, "{")
				index.uiPrefixed = append(index.uiPrefixed, uiPrefix{prefix, rt.uiSub})
			}
		}
	})
	return index
}

type routeIndex struct {
	usage      map[string]string
	uiExact    map[string][]string
	uiPrefixed []uiPrefix
}

type uiPrefix struct {
	prefix string
	allow  func(rest, method string) bool
}

var (
	indexOnce sync.Once
	index     *routeIndex
)

// usageRoutes names what is counted, by the route that served it, from the table.
func usageRoutes() map[string]string { return tableIndex().usage }

// methodGate answers 405, with Allow, to a method the route doesn't take, as each handler did for
// itself before.
func methodGate(methods []string, h http.HandlerFunc) http.HandlerFunc {
	allow := strings.Join(methods, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(methods, r.Method) {
			httpx.MethodNotAllowed(w, r, allow)
			return
		}
		h(w, r)
	}
}
