package gateway

import (
	"net/http"
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
		{pattern: "/_localbase/host", handler: s.guard("models:read", s.hostAPI)},
		{pattern: "/_localbase/computer-name", handler: s.auditedRoute("computer-name", computerNameChange, s.computerNameAPI)},
		{pattern: "/_localbase/default-model", handler: s.defaultModel}, // for the local CLI: an empty 404 without its instance token, by design
		{pattern: "/_localbase/placement-state", handler: s.placementRuntimeState},
		{pattern: "/v1/models", handler: s.guard("models:read", s.listModels)},
		{pattern: "/v1/models/{model...}", handler: s.guard("models:read", s.retrieveModel)},
		{pattern: "/v1/chat/completions", handler: s.guard("inference:chat", s.chat), usage: "chat"},
		{pattern: "/v1/embeddings", handler: s.guard("inference:embeddings", s.embeddings), usage: "embeddings"},
		{pattern: "/v1/systemone", handler: s.guard("inference:decision", s.systemone), usage: "decision"},
		{pattern: "/v1/audio/transcriptions", handler: s.guard("inference:transcription", s.transcription), usage: "transcription"},
		{pattern: "/v1/audio/translations", handler: s.guard("inference:transcription", s.transcription), usage: "translation"},
		{pattern: "/v1/audio/speech", handler: s.guard("inference:speech", s.speech), usage: "speech"},
		{pattern: "/v1/images/generations", handler: s.guard("inference:image", s.imageGeneration), usage: "image"},
		{pattern: "/v1/images/edits", handler: s.guard("inference:image", s.imageEdit), usage: "image-edit"},
		{pattern: "/v1/videos", handler: s.guard("inference:video", s.videoCreate), usage: "video"},
		{pattern: "/v1/videos/{job_id}", handler: s.videoJobRoute("status")},
		{pattern: "/v1/videos/{job_id}/content", handler: s.videoJobRoute("content")},
		{pattern: "/v1/videos/{job_id}/cancel", handler: s.videoJobRoute("cancel")},
		{pattern: "/_localbase/runs", handler: s.guard("", s.runsList)},
		{pattern: "/_localbase/runs/{rest...}", handler: s.guard("", s.runRoute)},
		{pattern: "/_localbase/models", handler: s.guard("models:read", s.modelMetadata)},
		{pattern: "/_localbase/models/{model_id...}", handler: s.auditedModelRoute(func(w http.ResponseWriter, r *http.Request) {
			s.guard(modelRoutePermission(r), s.modelMetadata)(w, r)
		})},
		{pattern: "/_localbase/access-management", handler: s.audited("access", s.accessManagement)},
		{pattern: "/_localbase/api-keys", handler: s.audited("api-key", s.apiKeys)},
		{pattern: "/_localbase/hf-token", handler: s.auditedRoute("hf-token", hfTokenChange, s.hfToken)},
		{pattern: "/_localbase/backends", handler: s.backendsAPI},
		{pattern: "/_localbase/model-backend", handler: s.audited("model-backend", s.modelBackendAPI)},
		{pattern: "/_localbase/model-management", handler: s.auditedRoute("models", modelsChange, s.modelManagementAPI)},
		{pattern: "/_localbase/default-chat-models", handler: s.auditedRoute("default-models", defaultModelsChange, s.defaultChatModelsAPI)},
		{pattern: "/_localbase/model-discovery", handler: s.auditedRoute("discovery", discoveryChange, s.modelDiscoveryAPI)},
		{pattern: "/_localbase/model-imports", handler: s.auditedRoute("imports", importsChange, s.modelImportsAPI)},
		{pattern: "/_localbase/auth-profiles", handler: s.authProfilesAPI},
		{pattern: "/_localbase/local-sessions", handler: s.localSessionsAPI},
		{pattern: "/_localbase/usage", handler: s.usageAPI},
		{pattern: "/_localbase/update", handler: s.updateAPI},
		{pattern: "/_localbase/voices", handler: s.auditedRoute("voices", voicesChange, s.voicesAPI)},
		{pattern: "/_localbase/voices/", handler: s.auditedRoute("voices", voicesChange, s.voicesAPI)},
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

// usageRoutes names what is counted, by the route that served it, from the table. It is built on
// first use, not as a package variable: the table's handlers lead back to recordUsage, which reads it.
func usageRoutes() map[string]string {
	usageOnce.Do(func() {
		usageLabels = map[string]string{}
		for _, rt := range (&Server{}).routeTable() { // only to read the table: no handler runs
			if rt.usage != "" {
				usageLabels[rt.pattern] = rt.usage
			}
		}
	})
	return usageLabels
}

var (
	usageOnce   sync.Once
	usageLabels map[string]string
)
