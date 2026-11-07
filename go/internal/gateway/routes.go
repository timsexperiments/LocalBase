package gateway

import (
	"net/http"
	"strings"
	"sync"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
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
}

// routeTable is every path the gateway serves, in the order they are registered.
func (s *Server) routeTable() []route {
	routes := []route{
		{pattern: "/health", handler: s.health},
		{pattern: "/openapi.json", handler: s.openAPI},
		{pattern: "/app/login", handler: s.uiAccess},
		{pattern: auth.LocalOncePath, handler: s.uiAccess},
		{pattern: "/oidc/callback", handler: s.uiAccess},
		{pattern: "/github/callback", handler: s.uiAccess},
		{pattern: "/app/login/magic", handler: s.uiAccess},
		{pattern: "/magic-link/callback", handler: s.uiAccess},
		{pattern: "/app/logout", handler: s.uiAccess},
		{pattern: "/app/session", handler: s.uiAccess},
		{pattern: "/app/api", handler: s.uiAccess},
		{pattern: "/app/api/", handler: s.uiAccess},
		{pattern: "/health/ready", handler: s.ready},
		{pattern: "/_localbase/instance", handler: s.instance},
		{pattern: "/_localbase/host", handler: s.guard("models:read", s.hostAPI), ui: []string{"GET"}},
		{pattern: "/_localbase/computer-name", handler: s.auditedRoute("computer-name", computerNameChange, s.computerNameAPI), ui: []string{"PUT"}},
		{pattern: "/_localbase/default-model", handler: s.defaultModel}, // for the local CLI: an empty 404 without its instance token, by design
		{pattern: "/_localbase/placement-state", handler: s.placementRuntimeState},
		{pattern: "/v1/models", handler: s.guard("models:read", s.listModels)},
		{pattern: "/v1/models/{model...}", handler: s.guard("models:read", s.retrieveModel)},
		{pattern: "/v1/chat/completions", handler: s.guard("inference:chat", s.chat), usage: "chat", ui: []string{"POST"}},
		{pattern: "/v1/embeddings", handler: s.guard("inference:embeddings", s.embeddings), usage: "embeddings", ui: []string{"POST"}},
		{pattern: "/v1/systemone", handler: s.guard("inference:decision", s.systemone), usage: "decision", ui: []string{"POST"}},
		{pattern: "/v1/audio/transcriptions", handler: s.guard("inference:transcription", s.transcription), usage: "transcription", ui: []string{"POST"}},
		{pattern: "/v1/audio/translations", handler: s.guard("inference:transcription", s.transcription), usage: "translation"},
		{pattern: "/v1/audio/speech", handler: s.guard("inference:speech", s.speech), usage: "speech", ui: []string{"POST"}},
		{pattern: "/v1/images/generations", handler: s.guard("inference:image", s.imageGeneration), usage: "image", ui: []string{"POST"}},
		{pattern: "/v1/images/edits", handler: s.guard("inference:image", s.imageEdit), usage: "image-edit", ui: []string{"POST"}},
		{pattern: "/v1/videos", handler: s.guard("inference:video", s.videoCreate), usage: "video", ui: []string{"POST"}},
		{pattern: "/v1/videos/{job_id}", handler: s.videoJobRoute("status"), uiSub: uiVideoJob},
		{pattern: "/v1/videos/{job_id}/content", handler: s.videoJobRoute("content")},
		{pattern: "/v1/videos/{job_id}/cancel", handler: s.videoJobRoute("cancel")},
		{pattern: "/_localbase/runs", handler: s.guard("", s.runsList), ui: []string{"GET"}},
		{pattern: "/_localbase/runs/{rest...}", handler: s.guard("", s.runRoute), uiSub: uiRun},
		{pattern: "/_localbase/models", handler: s.guard("models:read", s.modelMetadata), ui: []string{"GET"}},
		{pattern: "/_localbase/models/{model_id...}", handler: s.auditedModelRoute(func(w http.ResponseWriter, r *http.Request) {
			s.guard(modelRoutePermission(r), s.modelMetadata)(w, r)
		}), uiSub: uiModel},
		{pattern: "/_localbase/access-management", handler: s.audited("access", s.accessManagement), ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/api-keys", handler: s.audited("api-key", s.apiKeys), ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/hf-token", handler: s.auditedRoute("hf-token", hfTokenChange, s.hfToken), ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/backends", handler: s.backendsAPI, ui: []string{"GET"}},
		{pattern: "/_localbase/model-backend", handler: s.audited("model-backend", s.modelBackendAPI), ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/model-management", handler: s.auditedRoute("models", modelsChange, s.modelManagementAPI), ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/default-chat-models", handler: s.auditedRoute("default-models", defaultModelsChange, s.defaultChatModelsAPI)},
		{pattern: "/_localbase/model-discovery", handler: s.auditedRoute("discovery", discoveryChange, s.modelDiscoveryAPI), ui: []string{"POST"}},
		{pattern: "/_localbase/model-imports", handler: s.auditedRoute("imports", importsChange, s.modelImportsAPI), ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/auth-profiles", handler: s.authProfilesAPI, ui: []string{"GET"}},
		{pattern: "/_localbase/local-sessions", handler: s.localSessionsAPI, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/usage", handler: s.usageAPI, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/update", handler: s.updateAPI, ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/voices", handler: s.auditedRoute("voices", voicesChange, s.voicesAPI), ui: []string{"GET", "POST"}},
		{pattern: "/_localbase/voices/", handler: s.auditedRoute("voices", voicesChange, s.voicesAPI), uiSub: uiVoice},
	}
	if s.UI != nil {
		routes = append(routes,
			route{pattern: "/app", handler: s.UI.ServeHTTP},
			route{pattern: "/app/", handler: s.UI.ServeHTTP},
			route{pattern: "/{$}", handler: s.UI.ServeHTTP},
		)
	}
	return append(routes, route{pattern: "/", handler: s.unmatched})
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
