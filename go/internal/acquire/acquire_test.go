package acquire

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
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
	"sync/atomic"
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
	p, err := Resolve(ctx, root, Request{Source: main.URL + "/files/model.gguf?sig=abc123", AuthProfile: "vendor", AllowHTTP: true, AllowPrivate: true, Confirmed: true, Task: "chat"})
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
	if strings.Contains(string(export), "abc123") {
		t.Fatal("the export contains a signed query value")
	}
	if strings.Contains(string(export), secret) {
		t.Fatal("the export contains the header value")
	}

	// A redirect to another origin does not carry the profile's headers.
	p, err = Resolve(ctx, root, Request{Source: main.URL + "/redirect/model.gguf", AuthProfile: "vendor", AllowHTTP: true, AllowPrivate: true, Confirmed: true, Task: "chat", ID: "redirected"})
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
	p, _ = Resolve(ctx, root, Request{Source: main.URL + "/files/model.gguf", AuthProfile: "vendor", AllowHTTP: true, AllowPrivate: true, Confirmed: true, Task: "chat",
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
	p, _ = Resolve(ctx, root, Request{Source: main.URL + "/files/model.gguf", AuthProfile: "vendor", AllowHTTP: true, AllowPrivate: true, Confirmed: true, Task: "chat",
		ID: "pinned", SHA256: sum(weights)})
	res, err = Import(ctx, root, p, nil)
	if err != nil || res.Model.Variants[0].Artifacts[0].HashTrust != models.HashUser {
		t.Fatalf("user checksum: %v", err)
	}

	// Errors never contain header values or signed query values.
	p, err = Resolve(ctx, root, Request{Source: main.URL + "/broken/model.gguf?token=topsecretquery", AuthProfile: "vendor", AllowHTTP: true, AllowPrivate: true, Confirmed: true, Task: "chat", ID: "broken"})
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
	p, err := Resolve(ctx, root, Request{Source: srv.URL + "/m.gguf", AllowHTTP: true, AllowPrivate: true, Confirmed: true, Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	// A previous attempt left the first 10,000 bytes.
	st := stagingDir(root, p)
	os.MkdirAll(st, 0o700)
	os.WriteFile(filepath.Join(st, "m.gguf.partial"), weights[:10000], 0o600)
	writeValidator(filepath.Join(st, "m.gguf.partial"), validator(testutil.Epoch.UTC().Format(http.TimeFormat)))
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
	if _, err := Resolve(ctx, root, Request{Source: filepath.Join(dir, "big-00001-of-00003.gguf"), AllowLocal: true, Task: "chat"}); err == nil ||
		!strings.Contains(err.Error(), "big-00003-of-00003.gguf") {
		t.Fatalf("incomplete local split: %v", err)
	}
	testutil.GGUF(filepath.Join(dir, "big-00003-of-00003.gguf"), map[string]any{"general.architecture": "qwen3", "split.count": 3, "split.no": 2}, 2048)
	p, err = Resolve(ctx, root, Request{Source: filepath.Join(dir, "big-00002-of-00003.gguf"), AllowLocal: true, Task: "chat"})
	if err != nil || len(p.Files) != 3 {
		t.Fatalf("local split: %v %+v", err, p)
	}
	res, err := Import(ctx, root, p, nil)
	if err != nil || res.Model.Variants[0].Architecture.Value != "qwen3" {
		t.Fatalf("local import: %v", err)
	}
	if _, err := Resolve(ctx, root, Request{Source: writeFile(t, "train.py"), AllowLocal: true}); err == nil {
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

// The Hugging Face token reaches a CDN origin only when the "huggingface"
// profile lists it; anonymous CDN downloads keep working without it.
func TestHFTokenCDNScope(t *testing.T) {
	weights := ggufBytes(t, "qwen3", 4096)
	cdnRec := &recorder{}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnRec.note(r)
		serveRange(w, r, weights)
	}))
	defer cdn.Close()
	const commit = "1111111111222222222233333333334444444444"
	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/revision/"):
			json.NewEncoder(w).Encode(map[string]any{"sha": commit, "pipeline_tag": "text-generation",
				"siblings": []any{map[string]any{"rfilename": "m.gguf", "lfs": map[string]any{"sha256": sum(weights), "size": len(weights)}}}})
		default:
			http.Redirect(w, r, cdn.URL+"/xet/m.gguf?sig=x", 302)
		}
	}))
	defer hf.Close()
	t.Setenv("HF_ENDPOINT", hf.URL)
	ctx := context.Background()
	root := t.TempDir()
	run := func(id, token string) {
		t.Helper()
		p, err := Resolve(ctx, root, Request{Source: "hf:org/m", HFToken: token, ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Import(ctx, root, p, nil); err != nil {
			t.Fatal(err)
		}
	}
	run("anonymous", "") // public repository, no token at all
	run("tokened", "hf_scopetest123")
	if cdnRec.anyHeader("hf_scopetest123") {
		t.Fatal("the token reached the CDN without being allowed")
	}
	if err := SaveProfile(root, Profile{ID: HFProfileID, Origins: []string{cdn.URL}}); err != nil {
		t.Fatal(err)
	}
	run("allowed", "hf_scopetest123")
	if !cdnRec.anyHeader("hf_scopetest123") {
		t.Fatal("the huggingface profile did not extend the token to its CDN origin")
	}
}

func TestRedirectDowngradeRefused(t *testing.T) {
	mk := func(u string) *http.Request { r, _ := http.NewRequest("GET", u, nil); return r }
	if err := redirectPolicy(mk("http://cdn.example.com/f"), []*http.Request{mk("https://src.example.com/f")}); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("https -> http: %v", err)
	}
	if err := redirectPolicy(mk("http://c.example.com/f"), []*http.Request{mk("https://a.example.com/f"), mk("https://b.example.com/f")}); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("https -> https -> http: %v", err)
	}
	if err := redirectPolicy(mk("https://b.example.com/f"), []*http.Request{mk("http://a.example.com/f")}); err != nil {
		t.Fatalf("http -> https is an upgrade: %v", err)
	}
}

// hostileGGUFs are headers that must never get past inspection.
func hostileGGUFs(t *testing.T) map[string][]byte {
	t.Helper()
	var hugeSplit, nested bytes.Buffer
	put := func(b *bytes.Buffer, vs ...any) {
		for _, v := range vs {
			binary.Write(b, binary.LittleEndian, v)
		}
	}
	put(&hugeSplit, uint32(0x46554747), uint32(3), uint64(0), uint64(1), uint64(len("split.count")))
	hugeSplit.WriteString("split.count")
	put(&hugeSplit, uint32(10), uint64(1_000_000_000_000))
	put(&nested, uint32(0x46554747), uint32(3), uint64(0), uint64(1), uint64(1))
	nested.WriteString("k")
	put(&nested, uint32(9))
	for range 1000 {
		put(&nested, uint32(9), uint64(1))
	}
	return map[string][]byte{"split-00001-of-00002.gguf": hugeSplit.Bytes(), "nested.gguf": nested.Bytes()}
}

// Through a URL import: the file downloads, is refused at inspection, and leaves
// nothing published.
func TestImportRefusesHostileHeaders(t *testing.T) {
	for name, body := range hostileGGUFs(t) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveRange(w, r, body) }))
			defer srv.Close()
			ctx := context.Background()
			p, err := Resolve(ctx, root, Request{Source: srv.URL + "/" + name, AllowHTTP: true, AllowPrivate: true, ID: "hostile", Task: "chat"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = Import(ctx, root, p, nil)
			if err == nil || !strings.Contains(err.Error(), "not published") {
				t.Fatalf("%v, want a refusal", err)
			}
			if _, err := os.Stat(ImportedDir(root, "hostile")); err == nil {
				t.Error("the import was published")
			}
			if _, err := os.Stat(filepath.Join(stagingDir(root, p), name)); err == nil {
				t.Error("the refused file stayed in staging")
			}
		})
	}
}

// Through a local path: refused while resolving, before anything is copied.
func TestResolveLocalRefusesHostileHeaders(t *testing.T) {
	for name, body := range hostileGGUFs(t) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if name == "split-00001-of-00002.gguf" {
				// The other part is present, so only the header can refuse it.
				if err := os.WriteFile(filepath.Join(dir, "split-00002-of-00002.gguf"), body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Resolve(context.Background(), t.TempDir(), Request{Source: path, AllowLocal: true, ID: "hostile", Task: "chat"})
			if err == nil || !strings.Contains(err.Error(), "cannot be imported") {
				t.Fatalf("%v, want a refusal", err)
			}
		})
	}
}

// By default an import reaches public addresses only, including through a redirect,
// and the opt-in that lifts this is not something a request can set for itself.
func TestImportsKeepToPublicAddresses(t *testing.T) {
	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		if strings.Contains(r.URL.Path, "away") {
			http.Redirect(w, r, "http://127.0.0.1:"+strings.Split(r.Host, ":")[1]+"/m.gguf", http.StatusFound)
			return
		}
		serveRange(w, r, ggufBytes(t, "qwen3", 2048))
	}))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	root := t.TempDir()
	ctx := context.Background()
	for _, source := range []string{srv.URL + "/m.gguf", "http://localhost:" + port + "/m.gguf", "http://[::1]:" + port + "/m.gguf",
		"http://[::ffff:127.0.0.1]:" + port + "/m.gguf", "http://127.1:" + port + "/m.gguf", "http://2130706433:" + port + "/m.gguf",
		"http://169.254.169.254/latest/meta-data/m.gguf", "http://[fd00:ec2::254]/m.gguf", "http://10.0.0.1/m.gguf", "http://192.168.1.1/m.gguf"} {
		for _, confirmed := range []bool{false, true} {
			seen.Store(0)
			_, err := Resolve(ctx, root, Request{Source: source, AllowHTTP: true, Confirmed: confirmed, ID: "x", Task: "chat"})
			var refused *RefusedError
			if !errors.As(err, &refused) || seen.Load() != 0 {
				t.Errorf("%s (confirmed %v): %v, reached %d", source, confirmed, err, seen.Load())
			}
		}
	}
	code := func(req Request) (string, string) {
		_, err := Resolve(ctx, root, req)
		var refused *RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("%+v: %v", req, err)
		}
		return refused.Code, refused.Message
	}
	// Whoever asks through the API or a page is told what to do, without settings.
	c, msg := code(Request{Source: srv.URL + "/m.gguf", AllowHTTP: true, ID: "x", Task: "chat"})
	if c != CodePrivateAddress || strings.Contains(msg, "allow-private") || strings.Contains(msg, "127.0.0.1") || !strings.Contains(msg, "Ask whoever runs") {
		t.Errorf("%s: %s", c, msg)
	}
	// The person at this computer is told how to allow it.
	c, msg = code(Request{Source: srv.URL + "/m.gguf", AllowHTTP: true, OnHost: true, ID: "x", Task: "chat"})
	if c != CodePrivateAddress || !strings.Contains(msg, "--allow-private") || !strings.Contains(msg, "--allow-private-imports") {
		t.Errorf("on the host %s: %s", c, msg)
	}
	// What nothing can allow offers no way to allow it, even with the opt-in.
	for _, onHost := range []bool{false, true} {
		c, msg = code(Request{Source: "http://169.254.169.254/m.gguf", AllowHTTP: true, AllowPrivate: true, OnHost: onHost, ID: "x", Task: "chat"})
		if c != CodeAddressRefused || strings.Contains(msg, "allow") {
			t.Errorf("metadata, on host %v: %s: %s", onHost, c, msg)
		}
	}
	// With the opt-in a LAN source works, end to end.
	p, err := Resolve(ctx, root, Request{Source: srv.URL + "/m.gguf", AllowHTTP: true, AllowPrivate: true, Confirmed: true, ID: "lan", Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Import(ctx, root, p, nil); err != nil {
		t.Fatal(err)
	}
}

// A redirect to a private address is refused while downloading, as well as while
// resolving: a plan can outlive what its source answered when it was made.
func TestImportRedirectToPrivateIsRefused(t *testing.T) {
	var private atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		private.Add(1)
		serveRange(w, r, ggufBytes(t, "qwen3", 2048))
	}))
	defer target.Close()
	// HF_ENDPOINT names this host, so the first hop is allowed and the second is judged.
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(target.URL, "127.0.0.1", "localhost", 1)+"/m.gguf", http.StatusFound)
	}))
	defer mirror.Close()
	t.Setenv("HF_ENDPOINT", mirror.URL)
	_, err := Resolve(context.Background(), t.TempDir(), Request{Source: mirror.URL + "/redirect/m.gguf", AllowHTTP: true, Confirmed: true, ID: "x", Task: "chat"})
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Code != CodePrivateAddress || private.Load() != 0 {
		t.Fatalf("%v, reached %d", err, private.Load())
	}
}

// Nothing contacts a custom source before the person has accepted its warning.
func TestCustomSourceIsNotContactedBeforeConfirmation(t *testing.T) {
	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		serveRange(w, r, ggufBytes(t, "qwen3", 2048))
	}))
	defer srv.Close()
	ctx := context.Background()
	p, err := Resolve(ctx, t.TempDir(), Request{Source: srv.URL + "/m.gguf", AllowHTTP: true, AllowPrivate: true, ID: "x", Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Load() != 0 {
		t.Errorf("contacted %d times before the confirmation", seen.Load())
	}
	if !p.Custom || p.SizeKnown || len(p.Files) != 1 || p.Files[0].Filename != "m.gguf" {
		t.Errorf("plan %+v", p)
	}
	p, err = Resolve(ctx, t.TempDir(), Request{Source: srv.URL + "/m.gguf", AllowHTTP: true, AllowPrivate: true, Confirmed: true, ID: "x", Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Load() != 1 || !p.SizeKnown || p.TotalBytes != 2048 {
		t.Errorf("after the confirmation: contacted %d, size known %v, %d bytes", seen.Load(), p.SizeKnown, p.TotalBytes)
	}
}

// A local path needs the opt-in. An API caller cannot read the machine's files
// into the model store, or learn which exist.
func TestLocalPathsNeedTheOptIn(t *testing.T) {
	path := writeFile(t, "weights.gguf")
	ctx := context.Background()
	for _, source := range []string{path, "/no/such/file", "weights.gguf", "./weights.gguf", "../weights.gguf"} {
		// The same answer whether or not the file exists, and allowing a LAN mirror
		// does not change it.
		_, err := Resolve(ctx, t.TempDir(), Request{Source: source, AllowPrivate: true, ID: "x", Task: "chat"})
		var refused *RefusedError
		if !errors.As(err, &refused) || refused.Code != CodeLocalPath || refused.Message != localPathMessage {
			t.Errorf("%s: %v", source, err)
		}
	}
	if _, err := Resolve(ctx, t.TempDir(), Request{Source: path, AllowLocal: true, ID: "x", Task: "chat"}); err != nil {
		t.Errorf("with the opt-in: %v", err)
	}
}

func TestParserTricksAreRefusedOrJudged(t *testing.T) {
	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen.Add(1) }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	ctx := context.Background()
	for _, source := range []string{"http://huggingface.co@127.0.0.1:" + port + "/m.gguf", "http://huggingface.co:443@127.0.0.1:" + port + "/m.gguf",
		"http://127.0.0.1:" + port + "/m.gguf#@huggingface.co", "http://127.0.0.1:" + port + "\\@huggingface.co/m.gguf",
		"http://user:pass@10.0.0.1/m.gguf", "http://127.0.0.1:" + port + "?@huggingface.co/m.gguf"} {
		if _, err := Resolve(ctx, t.TempDir(), Request{Source: source, AllowHTTP: true, Confirmed: true, ID: "x", Task: "chat"}); err == nil {
			t.Errorf("%s was accepted", source)
		}
	}
	if seen.Load() != 0 {
		t.Errorf("reached %d times", seen.Load())
	}
}
