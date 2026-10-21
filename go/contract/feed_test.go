package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/runtime"
)

// testFeed builds a v2 feed: a community model, an entry for a built-in id
// (left out), an invalid entry (skipped), and annotations (one invalid).
// encoding/json writes object keys sorted, which is also how JSON.stringify
// reads them back, so the checksum is what both implementations compute.
func testFeed(t *testing.T) []byte {
	t.Helper()
	community := map[string]any{
		"modelId": "community-tiny-stt", "displayName": "Community Tiny", "kind": "stt", "provider": "Community", "family": "Whisper",
		"version": "test", "size": "tiny", "quant": "Q8_0", "storageGb": 0.01,
		"source": "https://huggingface.co/community/tiny", "repositoryRevision": strings.Repeat("b", 40),
		"artifacts": []any{map[string]any{"sourcePath": "ggml-community-tiny.bin", "filename": "ggml-community-tiny.bin",
			"expectedSizeBytes": 4096, "sha256": strings.Repeat("c", 64), "role": "primary"}},
		"inputModalities": []string{"audio"}, "outputModalities": []string{"text"}, "capabilities": []string{"speech-to-text"},
		"commercialStatus": "open", "catch": "Community entry.", "notes": "From the test feed.",
	}
	builtIn := map[string]any{"modelId": "whisper-tiny-en-q8_0", "notes": "ignored"}
	invalid := map[string]any{"modelId": "broken-entry", "kind": "stt"}
	cat, _ := catalog.LoadWithFormer()
	coder := cat.ByID("qwen2.5-coder-3b-instruct-q4_k_m")
	peak := func(shape string, bytes int64) map[string]any {
		return map[string]any{"variant": "Q8_0", "launchShape": shape, "peakBytes": bytes, "backendAcceleratorBytes": bytes,
			"hostBufferBytes": 0, "peakRssBytes": bytes / 2, "measuredAt": "2026-10-15", "hardware": "Test rig"}
	}
	mark := func(id string) map[string]any {
		return map[string]any{"id": id, "supported": false, "source": "declared", "verified": true, "reason": "Verified missing in the test feed."}
	}
	annotations := []any{
		// Built-in models without a checked-in measurement: the feed peak and
		// mark apply on both gateways.
		map[string]any{"repo": "ggerganov/whisper.cpp", "revision": "5359861c739e955e79d9a303bcbc70fb988958b1", "file": "ggml-small-q8_0.bin",
			"modelId": "whisper-small-q8_0", "kind": "stt", "status": "verified", "memoryPeaks": []any{peak("default", 3_100_000_000)},
			"unsupportedCapabilities": []any{mark("translation")}},
		map[string]any{"repo": "Qwen/Qwen2.5-Coder-3B-Instruct-GGUF", "revision": "f74adce6aa16316c625447af059dbebe4983757c",
			"file": "qwen2.5-coder-3b-instruct-q4_k_m.gguf", "modelId": coder.ModelID, "kind": "llm", "status": "verified",
			"memoryPeaks": []any{peak(runtime.ReferenceShape(coder, nil), 60_000_000_000)}, "unsupportedCapabilities": []any{mark("tool-calling")}},
		map[string]any{"repo": "community/tiny", "revision": strings.Repeat("b", 40), "file": "ggml-community-tiny.bin",
			"modelId": "community-tiny-stt", "status": "verified"},
		map[string]any{"repo": "not a repo", "revision": "x"},
	}
	body := map[string]any{"annotations": annotations, "models": []any{community, builtIn, invalid}}
	b, _ := json.Marshal(body)
	sum := sha256.Sum256(b)
	doc := map[string]any{"format": "localbase-catalog-feed", "schemaVersion": 2, "minReaderVersion": 2,
		"sha256": hex.EncodeToString(sum[:]), "annotations": annotations, "models": body["models"]}
	out, _ := json.Marshal(doc)
	return out
}

// TestCLIFeed compares `models feed` on twin roots against one local feed.
func TestCLIFeed(t *testing.T) {
	requireTS(t)
	feed := testFeed(t)
	var hits atomic.Int32
	srv := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/feed.json":
			w.Header().Set("ETag", `"v1"`)
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Write(feed)
		case "/broken.json":
			w.Write([]byte(`{"format":"localbase-catalog-feed","schemaVersion":2,"sha256":"` + strings.Repeat("0", 64) + `","models":[]}`))
		default:
			http.NotFound(w, r)
		}
	}), false)
	ts, gr := twinRoots(t)
	roots := [2]string{ts, gr}
	for i, impl := range []string{"ts", "go"} {
		if r := runCLI(t, impl, roots[i], "--json", "init"); r.code != 0 {
			t.Fatalf("%s init: %s %s", impl, r.stdout, r.stderr)
		}
	}
	// The feed variable unset, so the root's settings decide.
	env := []string{"LOCALBASE_CATALOG_FEED_URL="}
	compare := func(args ...string) {
		t.Helper()
		var outs [2]cliResult
		for i, impl := range []string{"ts", "go"} {
			outs[i] = runCLIEnv(t, impl, roots[i], env, args...)
		}
		label := strings.Join(args, " ")
		if outs[0].code != outs[1].code {
			t.Errorf("%s: exit ts=%d go=%d\nts: %s%s\ngo: %s%s", label, outs[0].code, outs[1].code, outs[0].stdout, outs[0].stderr, outs[1].stdout, outs[1].stderr)
			return
		}
		a, b := normalize(t, outs[0].stdout, roots[0]), normalize(t, outs[1].stdout, roots[1])
		if d := diffJSON("", a, b, func(p string) bool { return p == ".error.message" }); len(d) > 0 {
			if len(d) > 20 {
				d = d[:20]
			}
			t.Errorf("%s differs:\n  %s", label, strings.Join(d, "\n  "))
		}
	}
	feedURL := srv.URL + "/feed.json"
	compare("--json", "models", "feed", "status")
	compare("--json", "models", "feed", "use", "ftp://example.com/feed.json")
	compare("--json", "models", "feed", "use", feedURL)
	compare("--json", "models", "feed", "status")
	compare("--json", "models", "feed", "refresh")
	compare("--json", "models", "feed", "status")
	compare("--json", "models", "feed", "refresh")
	compare("--json", "models", "feed", "refresh", "--force")
	compare("--json", "models", "show", "community-tiny-stt")
	compare("--json", "models", "catalog", "--kind", "stt")
	compare("--json", "models", "show", "whisper-small-q8_0")
	compare("--json", "models", "show", "qwen2.5-coder-3b-instruct-q4_k_m")
	compare("--json", "models", "catalog", "--kind", "llm")
	compare("--json", "models", "catalog", "--capability", "tool-calling")
	compare("--json", "models", "recommend", "--vram", "16")
	for _, id := range []string{"whisper-small-q8_0", "qwen2.5-coder-3b-instruct-q4_k_m", "community-tiny-stt"} {
		var text [2]string
		for i, impl := range []string{"ts", "go"} {
			text[i] = strings.ReplaceAll(runCLIEnv(t, impl, roots[i], env, "models", "show", id).stdout, roots[i], "<root>")
		}
		if text[0] != text[1] {
			t.Errorf("models show %s text differs:\n--- ts\n%s\n--- go\n%s", id, text[0], text[1])
		}
	}
	compare("--json", "models", "feed", "use", srv.URL+"/broken.json")
	compare("--json", "models", "feed", "refresh")
	compare("--json", "models", "feed", "status")
	compare("--json", "models", "feed", "use", "off")
	compare("--json", "models", "feed", "status")
	compare("--json", "models", "feed", "use", "default")
	if hits.Load() == 0 {
		t.Fatal("the feed was never requested")
	}
}

// TestFeedSchedulerReloadsCatalog: a gateway downloads its feed at start and
// serves the community model without a restart (Go; TS does the same).
func TestFeedSchedulerReloadsCatalog(t *testing.T) {
	if *implFlag != "go" && *implFlag != "both" {
		t.Skip("runs the Go gateway")
	}
	feed := testFeed(t)
	srv := rangeServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(feed) }), false)
	tmp := scratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true,
		env: []string{"LOCALBASE_CATALOG_FEED_URL=" + srv.URL + "/feed.json"}}
	if r := e.cli(nil, "--json", "init"); r.code != 0 {
		t.Fatalf("init: %s %s", r.stdout, r.stderr)
	}
	e.serve("--no-auth")
	defer e.stop()
	deadline := time.Now().Add(15 * time.Second)
	for {
		r := e.do("GET", "/_localbase/models/community-tiny-stt", "")
		if r.Status == 200 {
			c := r.JSON(t)["catalog"].(map[string]any)
			if c["catalogTier"] != "remote" || c["trust"] != "community" || c["feedAnnotation"] == nil {
				t.Fatalf("community model: %s", r.Body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the feed was not applied: %d %s", r.Status, r.Body)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// The download is cached for the next process.
	if _, err := os.Stat(filepath.Join(e.root, "cache", "catalog-feed", "feed.json")); err != nil {
		t.Fatal(err)
	}
}
