package gateway

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// oldAllowedUIRoute is allowedUIRoute as it was written by hand, before the route table (wave 2,
// step 4b). The browser's reach through /app/api is a security boundary, so this copy stays for good
// as the oracle the table-built allowedUIRoute is checked against: a change to what the browser may
// call has to change both, on purpose.
func oldAllowedUIRoute(method, p string) bool {
	switch p {
	case "/_localbase/access-management", "/_localbase/api-keys", "/_localbase/model-management", "/_localbase/hf-token", "/_localbase/local-sessions", "/_localbase/usage", "/_localbase/update":
		return method == "GET" || method == "POST"
	case "/_localbase/model-backend", "/_localbase/model-imports":
		return method == "GET" || method == "POST"
	case "/_localbase/backends", "/_localbase/auth-profiles", "/_localbase/host":
		return method == "GET"
	case "/_localbase/computer-name":
		return method == "PUT"
	case "/_localbase/model-discovery":
		return method == "POST"
	case "/_localbase/models":
		return method == "GET"
	case "/_localbase/runs":
		return method == "GET"
	case "/_localbase/voices":
		return method == "GET" || method == "POST"
	case "/v1/chat/completions", "/v1/embeddings", "/v1/systemone", "/v1/images/generations", "/v1/images/edits",
		"/v1/audio/speech", "/v1/audio/transcriptions", "/v1/videos":
		return method == "POST"
	}
	if rest, ok := strings.CutPrefix(p, "/_localbase/voices/"); ok && rest != "" {
		if strings.HasSuffix(rest, "/audio") && strings.Count(rest, "/") == 1 {
			return method == "GET"
		}
		if !strings.Contains(rest, "/") {
			return method == "GET" || method == "PATCH" || method == "DELETE"
		}
		return false
	}
	if rest, ok := strings.CutPrefix(p, "/_localbase/models/"); ok {
		if id, ok := strings.CutSuffix(rest, "/unload"); ok && id != "" && !strings.Contains(id, "/") {
			return method == "POST"
		}
		if id, ok := strings.CutSuffix(rest, "/admission"); ok && id != "" && !strings.Contains(id, "/") {
			return method == "GET"
		}
	}
	if rest, ok := strings.CutPrefix(p, "/_localbase/runs/"); ok && rest != "" {
		if target, ok := strings.CutPrefix(rest, "v1/"); ok {
			_, known := Targets["/v1/"+target]
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
	if rest, ok := strings.CutPrefix(p, "/v1/videos/"); ok && rest != "" {
		switch {
		case strings.HasSuffix(rest, "/content"):
			return method == "GET"
		case strings.HasSuffix(rest, "/cancel"):
			return method == "POST"
		case !strings.Contains(rest, "/"):
			return method == "GET" || method == "DELETE"
		}
	}
	return false
}

// The browser's allow-list built from the route table answers exactly as the hand-written one: for
// every registered route and the matrix's concrete paths, every method, and a set of odd paths
// around each (slashes, case, dots, encoded separators, suffixes that belong to other routes,
// paths deeper than any route).
func TestTheUIAllowListIsTheOracle(t *testing.T) {
	methods := append(slices.Clone(matrixMethods), "get", "PROPFIND")
	base := map[string]bool{"": true, "/": true}
	for p := range matrixPaths(gatewayPatterns(t)) {
		base[p] = true
	}
	for _, extra := range matrixExtraPaths["/app/api/"] {
		base["/"+extra] = true
	}
	for _, p := range oraclePaths(t) {
		base[p] = true
	}
	for target := range Targets {
		base["/_localbase/runs"+target] = true
	}
	for _, p := range []string{"/_localbase/voices/", "/_localbase/models/", "/_localbase/runs/", "/v1/videos/", "/_localbase/runs/v1/", "/_localbase/runs/v1/nope"} {
		base[p] = true
	}
	suffixes := []string{"", "/", "//", "/audio", "/unload", "/admission", "/placement", "/events", "/cancel", "/result", "/content", "/x/audio", "/x/unload", "/x/admission", "/a/b", "/a/b/c", "/.", "/..", "/%2F", "?x=1"}
	paths := map[string]bool{}
	for p := range base {
		for _, s := range suffixes {
			paths[p+s] = true
		}
		paths["/"+p] = true
		paths[strings.ToUpper(p)] = true
		paths[strings.ReplaceAll(p, "/", "%2F")] = true
		paths[strings.Replace(p, "/", "/./", 1)] = true
		paths[strings.Replace(p, "/", "/../", 1)] = true
		paths[strings.TrimSuffix(p, "/")] = true
	}
	allowed, checked := 0, 0
	for p := range paths {
		for _, m := range methods {
			want := oldAllowedUIRoute(m, p)
			if got := allowedUIRoute(m, p); got != want {
				t.Errorf("%s %q: %v, the hand-written list says %v", m, p, got, want)
			}
			checked++
			if want {
				allowed++
			}
		}
	}
	if allowed < 40 {
		t.Errorf("only %d of %d calls allowed: the cases no longer reach the list", allowed, checked)
	}
	t.Logf("%d calls checked, %d allowed", checked, allowed)
}

// oraclePaths are the exact paths the hand-written list names, read from its source.
func oraclePaths(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "ui_oracle_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		if b, ok := n.(*ast.BasicLit); ok && b.Kind == token.STRING {
			if s, err := strconv.Unquote(b.Value); err == nil && strings.HasPrefix(s, "/") {
				out = append(out, s)
			}
		}
		return true
	})
	if len(out) < 20 {
		t.Fatalf("found %d paths in the hand-written list", len(out))
	}
	return out
}
