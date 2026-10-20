package acquire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/models"
	"github.com/timsexperiments/LocalBase/go/internal/testutil"
)

const secret = "s3cr3t-header-value-9f1c"

// recorder notes every request each fake server saw.
type recorder struct {
	mu   sync.Mutex
	seen []*http.Request
}

func (r *recorder) note(req *http.Request) {
	r.mu.Lock()
	r.seen = append(r.seen, req.Clone(context.Background()))
	r.mu.Unlock()
}

func (r *recorder) anyHeader(value string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, q := range r.seen {
		for _, vs := range q.Header {
			for _, v := range vs {
				if strings.Contains(v, value) {
					return true
				}
			}
		}
	}
	return false
}

func ggufBytes(t *testing.T, arch string, size int) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "w.gguf")
	testutil.GGUF(p, map[string]any{"general.architecture": arch, "general.file_type": 15, arch + ".context_length": 8192}, size)
	b, _ := os.ReadFile(p)
	return b
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func serveRange(w http.ResponseWriter, r *http.Request, body []byte) {
	http.ServeContent(w, r, "", testutil.Epoch, strings.NewReader(string(body)))
}

// Acceptance: a public or authenticated Hugging Face import resolves an
// immutable revision; the token reaches only the Hugging Face origin.
func TestHuggingFaceImport(t *testing.T) {
	weights := ggufBytes(t, "qwen3", 8192)
	projector := ggufBytes(t, "clip", 2048)
	cdnRec, hfRec := &recorder{}, &recorder{}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnRec.note(r)
		switch {
		case strings.HasSuffix(r.URL.Path, "/model-Q4_K_M.gguf"):
			serveRange(w, r, weights)
		case strings.HasSuffix(r.URL.Path, "/mmproj-model-f16.gguf"):
			serveRange(w, r, projector)
		default:
			w.WriteHeader(404)
		}
	}))
	defer cdn.Close()
	const commit = "0123456789abcdef0123456789abcdef01234567"
	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hfRec.note(r)
		if r.Header.Get("Authorization") != "Bearer hf_testtoken123" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.URL.Path == "/api/models/org/model-GGUF/revision/main":
			json.NewEncoder(w).Encode(map[string]any{"sha": commit, "pipeline_tag": "image-text-to-text",
				"cardData": map[string]any{"license": "apache-2.0"},
				"siblings": []any{
					map[string]any{"rfilename": "model-Q4_K_M.gguf", "size": 0, "lfs": map[string]any{"sha256": sum(weights), "size": len(weights)}},
					map[string]any{"rfilename": "mmproj-model-f16.gguf", "lfs": map[string]any{"sha256": sum(projector), "size": len(projector)}},
					map[string]any{"rfilename": "modeling_custom.py", "size": 10},
					map[string]any{"rfilename": "README.md", "size": 10},
				}})
		case strings.HasPrefix(r.URL.Path, "/org/model-GGUF/resolve/"+commit+"/"):
			// Like the Hub: redirect to a CDN on another origin with a signed URL.
			http.Redirect(w, r, cdn.URL+"/blobs"+strings.TrimPrefix(r.URL.Path, "/org/model-GGUF/resolve/"+commit)+"?X-Amz-Signature=sig123secret", 302)
		default:
			w.WriteHeader(404)
		}
	}))
	defer hf.Close()
	t.Setenv("HF_ENDPOINT", hf.URL)
	root := t.TempDir()

	if _, err := Resolve(context.Background(), root, Request{Source: "hf:org/model-GGUF"}); err == nil || !strings.Contains(err.Error(), "gated or private") {
		t.Fatalf("without a token: %v", err)
	}
	p, err := Resolve(context.Background(), root, Request{Source: "hf:org/model-GGUF", HFToken: "hf_testtoken123"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Revision != commit || p.Task != "chat" || p.License != "apache-2.0" || len(p.Files) != 2 || p.Custom {
		t.Fatalf("plan %+v", p)
	}
	if p.Files[0].HashTrust != models.HashAuthoritative || p.Files[1].Role != models.RoleProjector || p.Files[1].Required {
		t.Fatalf("files %+v", p.Files)
	}
	var progress []Progress
	res, err := Import(context.Background(), root, p, func(pr Progress) { progress = append(progress, pr) })
	if err != nil {
		t.Fatal(err)
	}
	m := res.Model
	if m.Variants[0].Architecture.Value != "qwen3" || m.Variants[0].Artifacts[0].Source.Revision != commit ||
		!strings.Contains(strings.Join(m.Capabilities, ","), "vision") {
		t.Fatalf("model %+v", m)
	}
	if cdnRec.anyHeader("hf_testtoken123") {
		t.Fatal("the Hugging Face token was sent to the CDN origin")
	}
	b, _ := json.Marshal(progress)
	export, _ := m.Export()
	for _, text := range []string{string(b), string(export)} {
		if strings.Contains(text, "sig123secret") || strings.Contains(text, "hf_testtoken123") {
			t.Fatalf("a credential leaked: %s", text)
		}
	}
	if _, err := os.Stat(filepath.Join(ImportedDir(root, m.ID), "model-Q4_K_M.gguf")); err != nil {
		t.Fatal("not published")
	}
	// Repository code is never planned.
	if _, err := Resolve(context.Background(), root, Request{Source: "hf:org/model-GGUF", Files: []string{"modeling_custom.py"}, HFToken: "hf_testtoken123"}); err == nil {
		t.Fatal("planned a .py file")
	}
}

// Acceptance: a direct URL shows the custom-source warning and supports
// private headers without exposing them in progress, errors, exports or
// redirects; an unpinned URL records an observed hash; a trusted-checksum
// mismatch prevents publication.
func TestDirectURLImport(t *testing.T) {
	weights := ggufBytes(t, "qwen3", 16384)
	otherRec, mainRec := &recorder{}, &recorder{}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherRec.note(r)
		serveRange(w, r, weights)
	}))
	defer other.Close()
	main := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mainRec.note(r)
		if r.Header.Get("X-Api-Key") != secret {
			http.Error(w, "missing key", 403)
			return
		}
		switch r.URL.Path {
		case "/files/model.gguf":
			serveRange(w, r, weights)
		case "/redirect/model.gguf":
			http.Redirect(w, r, other.URL+"/model.gguf?token=signedvalue42", 302)
		case "/broken/model.gguf":
			http.Error(w, "nope", 500)
		}
	}))
	defer main.Close()
	root := t.TempDir()
	if err := SaveProfile(root, Profile{ID: "vendor", Origins: []string{main.URL}, Headers: map[string]string{"X-Api-Key": secret}}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(profilesPath(root)); info.Mode().Perm() != 0o600 {
		t.Fatalf("profile file mode %v", info.Mode().Perm())
	}
	ctx := context.Background()

	// Plain HTTP needs explicit consent and carries its own warning.
	if _, err := Resolve(ctx, root, Request{Source: main.URL + "/files/model.gguf", AuthProfile: "vendor"}); err == nil {
		t.Fatal("plain HTTP accepted without consent")
	}
	p, err := Resolve(ctx, root, Request{Source: main.URL + "/files/model.gguf?sig=abc123", AuthProfile: "vendor", AllowHTTP: true, Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	codes := []string{}
	for _, w := range p.Warnings {
		codes = append(codes, w.Code)
	}
	if !p.Custom || strings.Join(codes, ",") != "custom-source,insecure-http" || p.Warnings[0].Text != CustomSourceWarning {
		t.Fatalf("warnings %v", codes)
	}
	if p.TotalBytes != int64(len(weights)) || strings.Contains(p.Display, "abc123") {
		t.Fatalf("plan size/display: %d %s", p.TotalBytes, p.Display)
	}
	res, err := Import(ctx, root, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Model.Variants[0].Artifacts[0]
	if a.HashTrust != models.HashObserved || a.SHA256 != sum(weights) || a.Source.AuthProfile != "vendor" {
		t.Fatalf("artifact %+v", a)
	}
	export, _ := res.Model.Export()
	if strings.Contains(string(export), secret) {
		t.Fatal("the export contains the header value")
	}

	// A redirect to another origin does not carry the profile's headers.
	p, err = Resolve(ctx, root, Request{Source: main.URL + "/redirect/model.gguf", AuthProfile: "vendor", AllowHTTP: true, Task: "chat", ID: "redirected"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, root, p, nil); err != nil {
		t.Fatal(err)
	}
	if otherRec.anyHeader(secret) {
		t.Fatal("the private header followed a redirect to another origin")
	}
	if !mainRec.anyHeader(secret) {
		t.Fatal("the private header never reached its own origin")
	}

	// A wrong trusted checksum blocks publication.
	p, _ = Resolve(ctx, root, Request{Source: main.URL + "/files/model.gguf", AuthProfile: "vendor", AllowHTTP: true, Task: "chat",
		ID: "pinned", SHA256: strings.Repeat("ab", 32)})
	_, err = Import(ctx, root, p, nil)
	var mismatch *HashMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("mismatch: %v", err)
	}
	if _, err := os.Stat(ImportedDir(root, "pinned")); err == nil {
		t.Fatal("published despite a checksum mismatch")
	}
	// The right one is recorded as the user's.
	p, _ = Resolve(ctx, root, Request{Source: main.URL + "/files/model.gguf", AuthProfile: "vendor", AllowHTTP: true, Task: "chat",
		ID: "pinned", SHA256: sum(weights)})
	res, err = Import(ctx, root, p, nil)
	if err != nil || res.Model.Variants[0].Artifacts[0].HashTrust != models.HashUser {
		t.Fatalf("user checksum: %v", err)
	}

	// Errors never contain header values or signed query values.
	p, err = Resolve(ctx, root, Request{Source: main.URL + "/broken/model.gguf?token=topsecretquery", AuthProfile: "vendor", AllowHTTP: true, Task: "chat", ID: "broken"})
	if err == nil {
		_, err = Import(ctx, root, p, nil)
	}
	if err == nil || strings.Contains(err.Error(), "topsecretquery") || strings.Contains(err.Error(), secret) {
		t.Fatalf("error: %v", err)
	}
	// Reserved headers cannot be set by a profile.
	if err := SaveProfile(root, Profile{ID: "bad", Origins: []string{main.URL}, Headers: map[string]string{"Range": "bytes=0-"}}); err == nil {
		t.Fatal("a profile set Range")
	}
}

func TestResumeAndLocal(t *testing.T) {
	weights := ggufBytes(t, "qwen3", 64*1024)
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.Header.Get("Range"))
		serveRange(w, r, weights)
	}))
	defer srv.Close()
	root := t.TempDir()
	ctx := context.Background()
	p, err := Resolve(ctx, root, Request{Source: srv.URL + "/m.gguf", AllowHTTP: true, Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	// A previous attempt left the first 10,000 bytes.
	st := stagingDir(root, p)
	os.MkdirAll(st, 0o700)
	os.WriteFile(filepath.Join(st, "m.gguf.partial"), weights[:10000], 0o600)
	if _, err := Import(ctx, root, p, nil); err != nil {
		t.Fatal(err)
	}
	if ranges[len(ranges)-1] != "bytes=10000-" {
		t.Fatalf("did not resume: %v", ranges)
	}
	got, _ := os.ReadFile(filepath.Join(ImportedDir(root, p.ID), "m.gguf"))
	if sum(got) != sum(weights) {
		t.Fatal("resumed file differs")
	}

	// Local files: copied, split sets completed or refused.
	dir := t.TempDir()
	for i := 1; i <= 2; i++ {
		testutil.GGUF(filepath.Join(dir, fmt.Sprintf("big-%05d-of-00003.gguf", i)), map[string]any{"general.architecture": "qwen3", "split.count": 3, "split.no": i - 1}, 2048)
	}
	if _, err := Resolve(ctx, root, Request{Source: filepath.Join(dir, "big-00001-of-00003.gguf"), Task: "chat"}); err == nil ||
		!strings.Contains(err.Error(), "big-00003-of-00003.gguf") {
		t.Fatalf("incomplete local split: %v", err)
	}
	testutil.GGUF(filepath.Join(dir, "big-00003-of-00003.gguf"), map[string]any{"general.architecture": "qwen3", "split.count": 3, "split.no": 2}, 2048)
	p, err = Resolve(ctx, root, Request{Source: filepath.Join(dir, "big-00002-of-00003.gguf"), Task: "chat"})
	if err != nil || len(p.Files) != 3 {
		t.Fatalf("local split: %v %+v", err, p)
	}
	res, err := Import(ctx, root, p, nil)
	if err != nil || res.Model.Variants[0].Architecture.Value != "qwen3" {
		t.Fatalf("local import: %v", err)
	}
	if _, err := Resolve(ctx, root, Request{Source: writeFile(t, "train.py")}); err == nil {
		t.Fatal("imported a .py file")
	}
}

func writeFile(t *testing.T, name string) string {
	p := filepath.Join(t.TempDir(), name)
	os.WriteFile(p, []byte("print('hi')"), 0o644)
	return p
}

func TestRedactURL(t *testing.T) {
	got := RedactURL("https://user:pw@cdn.example.com/a/b.gguf?X-Amz-Signature=abc&Expires=1#frag")
	if got != "https://cdn.example.com/a/b.gguf?Expires=REDACTED&X-Amz-Signature=REDACTED" {
		t.Fatal(got)
	}
}
