package gateway

import (
	"slices"
	"strings"

	"github.com/timsexperiments/LocalBase/go/internal/gateway/runs"
)

// allowedUIRoute lists what the browser may call through /app/api: the route table's ui methods
// for an exact path, or its uiSub rule for a path under a pattern (ui_oracle_test.go keeps the list
// as it was written by hand, and checks this against it).
func allowedUIRoute(method, p string) bool {
	ix := tableIndex()
	if methods, ok := ix.uiExact[p]; ok {
		return slices.Contains(methods, method)
	}
	for _, sub := range ix.uiPrefixed {
		if rest, ok := strings.CutPrefix(p, sub.prefix); ok {
			return sub.allow(rest, method)
		}
	}
	return false
}

// uiVoice is what the browser may call under /_localbase/voices/.
func uiVoice(rest, method string) bool {
	if rest == "" {
		return false
	}
	if strings.HasSuffix(rest, "/audio") && strings.Count(rest, "/") == 1 {
		return method == "GET"
	}
	if !strings.Contains(rest, "/") {
		return method == "GET" || method == "PATCH" || method == "DELETE"
	}
	return false
}

// uiModel is what the browser may call under /_localbase/models/.
func uiModel(rest, method string) bool {
	if id, ok := strings.CutSuffix(rest, "/unload"); ok && id != "" && !strings.Contains(id, "/") {
		return method == "POST"
	}
	if id, ok := strings.CutSuffix(rest, "/admission"); ok && id != "" && !strings.Contains(id, "/") {
		return method == "GET"
	}
	return false
}

// uiRun is what the browser may call under /_localbase/runs/.
func uiRun(rest, method string) bool {
	if rest == "" {
		return false
	}
	if target, ok := strings.CutPrefix(rest, "v1/"); ok {
		_, known := runs.Targets["/v1/"+target]
		return known && method == "POST"
	}
	id, action, _ := strings.Cut(rest, "/")
	switch {
	case id == "":
		return false
	case action == "":
		return method == "GET" || method == "DELETE"
	case action == "events":
		return method == "GET"
	case action == "cancel":
		return method == "POST"
	case action == "result":
		return method == "GET"
	}
	return false
}

// uiVideoJob is what the browser may call under /v1/videos/.
func uiVideoJob(rest, method string) bool {
	if rest == "" {
		return false
	}
	switch {
	case strings.HasSuffix(rest, "/content"):
		return method == "GET"
	case strings.HasSuffix(rest, "/cancel"):
		return method == "POST"
	case !strings.Contains(rest, "/"):
		return method == "GET" || method == "DELETE"
	}
	return false
}
