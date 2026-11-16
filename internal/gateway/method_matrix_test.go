package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/auth"
)

// The method matrix pins, for every registered path and every method a client sends, what the
// gateway answers each of three callers: one with every permission, one with none, and one with no
// credentials at all. It pins the status, the Allow header and the error envelope's type and code.
// Routing and method gates are being rebuilt around one route table (wave 2, step 4b): none of
// these answers may change on the way unless a PR says why, and refusals must keep their order (a
// caller who may not use a route learns nothing about its methods).
// LOCALBASE_UPDATE_METHOD_MATRIX=1 rewrites the goldens from the gateway as it is.
var methodMatrixGoldens = map[string]string{
	"every permission": "testdata/method_matrix.golden",
	"no permission":    "testdata/method_matrix_no_permission.golden",
	"anonymous":        "testdata/method_matrix_anonymous.golden",
}

var pinnedMethods = []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost, http.MethodPut,
	http.MethodDelete, http.MethodPatch}

func TestEveryRouteAnswersEveryMethodAsPinned(t *testing.T) {
	g := newMatrixGateway(t, nil)
	_, full, err := g.store.CreateAPIKey(context.Background(), "everything", auth.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	paths := matrixPaths(gatewayPatterns(t))
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	slices.Sort(sorted)
	for _, caller := range []struct{ name, key string }{{"every permission", full}, {"no permission", g.noScopes}, {"anonymous", ""}} {
		t.Run(caller.name, func(t *testing.T) {
			headers := map[string]string{}
			if caller.key != "" {
				headers["Authorization"] = "Bearer " + caller.key
			}
			var got strings.Builder
			got.WriteString("# method\tpath\tstatus\tAllow\terror type\terror code\n")
			for _, path := range sorted {
				for _, method := range pinnedMethods {
					// No body: what is pinned is what a route says to a method, not what it does
					// with a request it takes.
					r := g.raw(t, method, path, "", headers, "")
					typ, code := envelopeKind(r.body)
					fmt.Fprintf(&got, "%s\t%s\t%d\t%s\t%s\t%s\n", method, path, r.status, dash(r.allow), dash(typ), dash(code))
				}
			}
			compareMethodMatrix(t, methodMatrixGoldens[caller.name], got.String())
		})
	}
	// Nothing was started, run or changed: every caller sent no body, and is refused or only reads.
	if s := g.stt.Snapshot().State; s != "idle" {
		t.Errorf("the speech-to-text runtime is %q", s)
	}
	if s := g.llm.Snapshot().State; s != "idle" {
		t.Errorf("the LLM runtime is %q", s)
	}
	if active, saved := g.runsLeft(t); active != 0 || saved != 0 {
		t.Errorf("runs exist: %d unfinished, %d saved", active, saved)
	}
	if keys, err := g.store.ListAPIKeys(context.Background()); err != nil || len(keys) != 2 {
		t.Errorf("API keys: %d, %v; want the two this test made", len(keys), err)
	}
}

// compareMethodMatrix compares got with a golden, or rewrites it under LOCALBASE_UPDATE_METHOD_MATRIX=1.
func compareMethodMatrix(t *testing.T, golden, got string) {
	t.Helper()
	if os.Getenv("LOCALBASE_UPDATE_METHOD_MATRIX") == "1" {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < max(len(gl), len(wl)); i++ {
			var a, b string
			if i < len(gl) {
				a = gl[i]
			}
			if i < len(wl) {
				b = wl[i]
			}
			if a != b {
				t.Errorf("answer changed:\n  now: %s\n  was: %s", a, b)
			}
		}
	}
}

// envelopeKind is the error envelope's type and code, in either shape the gateway sends
// ({"error":{"type","code"}} or a management {"error":{"code"}}), or none.
func envelopeKind(body string) (typ, code string) {
	var env struct {
		Error *struct {
			Type any `json:"type"`
			Code any `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &env) != nil || env.Error == nil {
		return "", ""
	}
	s := func(v any) string {
		if v == nil {
			return ""
		}
		return fmt.Sprint(v)
	}
	return s(env.Error.Type), s(env.Error.Code)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
