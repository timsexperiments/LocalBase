package system

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/testutil"
	"github.com/timsexperiments/LocalBase/go/test/harness"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

const searchModel = "search-tiny"

// addEmbeddingModel gives a scratch root an embedding model served by the fake backend.
func addEmbeddingModel(t *testing.T, e *e2e) {
	t.Helper()
	file := filepath.Join(e.root, "models", "llm", "search-tiny.gguf")
	if err := testutil.GGUF(file, map[string]any{"general.architecture": "backuptest", "general.file_type": 15, "backuptest.context_length": 4096}, 1<<16); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	entry, _ := json.Marshal(map[string]any{"modelId": searchModel, "displayName": "Search tiny", "kind": "llm", "provider": "LocalBase tests", "family": "Search",
		"version": "test", "size": "tiny", "quant": "Q8_0", "storageGb": 0.01, "source": "https://huggingface.co/localbase/search-tiny", "repositoryRevision": strings.Repeat("a", 40),
		"artifacts":       []any{map[string]any{"sourcePath": "search-tiny.gguf", "filename": "search-tiny.gguf", "expectedSizeBytes": len(data), "sha256": hex.EncodeToString(sum[:]), "role": "primary"}},
		"inputModalities": []string{"text"}, "outputModalities": []string{"text"}, "contextWindowTokens": 4096, "capabilities": []string{"embeddings"},
		"llmRuntime":       map[string]any{"capability": "embedding-only", "pooling": "last", "dimensions": map[string]any{"minimum": 64, "maximum": 64}},
		"commercialStatus": "open", "catch": "Test data.", "notes": "Contract fixture."})
	if err := fsguard.WriteFile(filepath.Join(e.root, "catalog.d", searchModel+".json"), entry, 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.cli(nil, "models", "backend", searchModel, "--set", "backup-fake"); r.code != 0 {
		t.Fatalf("set backend: %s%s", r.stdout, r.stderr)
	}
	// The API answers only for a selected model, so the page offers only those.
	show := e.cli(nil, "config", "show")
	lines := strings.Split(show.stdout, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "selectedLlmModels = ") {
			lines[i] = `selectedLlmModels = ["` + backupModel + `", "` + searchModel + `"]`
		}
	}
	cfg := filepath.Join(e.outDir, "search-config.toml")
	if err := fsguard.WriteFile(cfg, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.cli(nil, "config", "apply", "--file", cfg, "--restart", "never"); r.code != 0 {
		t.Fatalf("config apply: %s%s", r.stdout, r.stderr)
	}
}

func cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

type embeddingsAnswer struct {
	Object string `json:"object"`
	Model  string `json:"model"`
	Data   []struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
}

// signInLocally is a browser session on this computer, through a one-time link.
func signInLocally(t *testing.T, e *e2e) string {
	t.Helper()
	r := e.cli(nil, "--json", "access", "login-link")
	m := regexp.MustCompile(`#([A-Za-z0-9_-]+)`).FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil {
		t.Fatalf("login-link: %d %s %s", r.code, r.stdout, r.stderr)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest("POST", e.base+"/app/login/once", strings.NewReader(url.Values{"token": {m[1]}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 303 || len(resp.Cookies()) != 1 {
		t.Fatalf("redeem: %d", resp.StatusCode)
	}
	return resp.Cookies()[0].Name + "=" + resp.Cookies()[0].Value
}

// TestSearchContract holds what DESIGN §35 tells the page it can rely on, on real binaries and the
// fake backend: how it calls, what comes back, what it must handle, and that no text stays.
func TestSearchContract(t *testing.T) {
	backupGoOnly(t)
	tmp, _ := filepath.EvalSymlinks(fsguard.ScratchDir(t))
	e := backupRoot(t, tmp, "root")
	addEmbeddingModel(t, e)
	chatOnly := e.key
	e.key = e.json("keys", "create", "--name", "search", "--scopes", "inference:embeddings,models:read")["secret"].(string)
	e.serve()

	const (
		about   = "PLANTEDPASSAGE the river runs through the old mill"
		unlike  = "PLANTEDPASSAGE bread needs flour water and salt"
		another = "PLANTEDPASSAGE tax forms are due in the spring"
		query   = "PLANTEDQUERY where does the river run"
	)
	embed := func(headers map[string]string, base string, inputs ...string) *harness.Resp {
		body, _ := json.Marshal(map[string]any{"model": searchModel, "input": inputs})
		return harness.Do(t, "POST", base, headers, string(body))
	}
	vectors := func(t *testing.T, r *harness.Resp) [][]float64 {
		t.Helper()
		if r.Status != 200 {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		var a embeddingsAnswer
		if err := json.Unmarshal(r.Body, &a); err != nil {
			t.Fatal(err)
		}
		out := make([][]float64, len(a.Data))
		for i, d := range a.Data {
			if d.Index != i || d.Object != "embedding" {
				t.Errorf("entry %d: index %d, object %q", i, d.Index, d.Object)
			}
			out[i] = d.Embedding
		}
		return out
	}

	t.Run("a model that isn't selected doesn't exist to the API", func(t *testing.T) {
		body := `{"model":"backup-other-model","input":"x"}`
		if r := harness.Do(t, "POST", e.base+"/v1/embeddings", harness.Bearer(e.key), body); r.Status != 404 || !strings.Contains(string(r.Body), "model_not_found") {
			t.Errorf("%d %s", r.Status, r.Body)
		}
	})

	t.Run("the model is listed as embeddings", func(t *testing.T) {
		if r := e.do("GET", "/v1/models", ""); !strings.Contains(string(r.Body), searchModel) || !strings.Contains(string(r.Body), `"embeddings"`) {
			t.Errorf("%s", r.Body)
		}
	})

	var docs [][]float64
	t.Run("one vector per input, in order, and they rank", func(t *testing.T) {
		docs = vectors(t, embed(harness.Bearer(e.key), e.base+"/v1/embeddings", about, unlike, another))
		if len(docs) != 3 || len(docs[0]) == 0 {
			t.Fatalf("%v", docs)
		}
		q := vectors(t, embed(harness.Bearer(e.key), e.base+"/v1/embeddings", query))[0]
		if !(cosine(q, docs[0]) > cosine(q, docs[1]) && cosine(q, docs[0]) > cosine(q, docs[2])) {
			t.Errorf("the passage sharing the most words should rank first: %v %v %v", cosine(q, docs[0]), cosine(q, docs[1]), cosine(q, docs[2]))
		}
	})

	t.Run("a passage that is too long says so", func(t *testing.T) {
		r := embed(harness.Bearer(e.key), e.base+"/v1/embeddings", "fine", "fake:batch")
		if r.Status != 400 || !strings.Contains(string(r.Body), "context_length_exceeded") || !strings.Contains(string(r.Body), "this embedding model accepts") {
			t.Errorf("%d %s", r.Status, r.Body)
		}
	})

	t.Run("without the permission", func(t *testing.T) {
		if r := embed(harness.Bearer(chatOnly), e.base+"/v1/embeddings", "x"); r.Status != 403 {
			t.Errorf("%d %s", r.Status, r.Body)
		}
	})

	t.Run("from a page, through /app/api", func(t *testing.T) {
		page := map[string]string{"Cookie": signInLocally(t, e), "X-Localbase-Ui": "1", "Origin": e.base, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}
		if got := vectors(t, embed(page, e.base+"/app/api/v1/embeddings", about)); len(got) != 1 || cosine(got[0], docs[0]) < 0.999 {
			t.Errorf("the page should get the same vector: %v", got)
		}
		// Not same-origin: a page elsewhere can't use it.
		cross := map[string]string{"Cookie": page["Cookie"], "X-Localbase-Ui": "1", "Origin": "https://elsewhere.example.com", "Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"}
		if r := embed(cross, e.base+"/app/api/v1/embeddings", about); r.Status != 403 {
			t.Errorf("cross-site: %d", r.Status)
		}
	})

	// Keep this last: it needs the gateway stopped, so that everything is flushed.
	t.Run("no text stays anywhere in the root", func(t *testing.T) {
		e.stop()
		var scanned, bytesRead int
		err := filepath.WalkDir(e.root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			scanned++
			bytesRead += len(b)
			if strings.Contains(string(b), "PLANTEDPASSAGE") || strings.Contains(string(b), "PLANTEDQUERY") {
				t.Errorf("%s holds the text", p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if scanned < 3 {
			t.Errorf("only %d files scanned", scanned)
		}
		t.Logf("scanned %d files, %d bytes", scanned, bytesRead)
		// It is still counted, which is all usage ever holds.
		usage := e.cli(nil, "--json", "usage")
		var counted struct {
			Data struct {
				Totals struct {
					RequestsWithTokens int `json:"requestsWithTokens"`
				} `json:"totals"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(usage.stdout), &counted); err != nil || usage.code != 0 || counted.Data.Totals.RequestsWithTokens == 0 {
			t.Errorf("usage should count the requests: %d %v %s%s", usage.code, err, usage.stdout, usage.stderr)
		}
		if strings.Contains(usage.stdout, "PLANTED") {
			t.Error("usage holds the text")
		}
	})
}
