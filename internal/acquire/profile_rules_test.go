package acquire

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// writeProfileFile writes a root's profile file as someone editing it by hand would.
func writeProfileFile(t *testing.T, root, body string) {
	t.Helper()
	dir := filepath.Join(root, "credentials")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth-profiles.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadOrFail(t *testing.T, root string) *Profiles {
	t.Helper()
	ps, err := LoadProfiles(root)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// logRecords keeps what was logged.
type logRecords struct {
	mu  sync.Mutex
	got []slog.Record
}

func (l *logRecords) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecords) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, r)
	return nil
}
func (l *logRecords) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecords) WithGroup(string) slog.Handler      { return l }

// roundTripFunc answers a request without the network, so a test can use any host.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func noContent(r *http.Request) *http.Response {
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: r}
}

func hfRefusal(origin string) string {
	return fmt.Sprintf("origin %q isn't Hugging Face's own: the huggingface profile can only name huggingface.co, hf.co, their subdomains, or the origin HF_ENDPOINT names", origin)
}

// The huggingface profile names only Hugging Face's own hosts: huggingface.co, hf.co, or a subdomain of either at
// any depth, each label valid. The parsed host is compared whole, so a host that only starts or ends the same, a
// user, a port, another case or another alphabet doesn't pass.
func TestTheHuggingFaceProfileNamesOnlyHuggingFacesOwnHosts(t *testing.T) {
	t.Setenv("HF_ENDPOINT", "")
	check := func(origin string) error {
		p := Profile{ID: HFProfileID, Origins: []string{origin}}
		return p.Validate()
	}
	t.Run("accepted: Hugging Face's download host, a subdomain of a subdomain of hf.co", func(t *testing.T) {
		if err := check("https://cas-bridge.xethub.hf.co"); err != nil {
			t.Error(err)
		}
	})
	for name, o := range map[string]string{
		"huggingface.co":                       "https://huggingface.co",
		"hf.co":                                "https://hf.co",
		"a subdomain":                          "https://cdn-lfs.huggingface.co",
		"a subdomain of a subdomain":           "https://cdn-lfs.us-1.huggingface.co",
		"a trailing slash, as any profile may": "https://hf.co/",
	} {
		t.Run("accepted: "+name, func(t *testing.T) {
			if err := check(o); err != nil {
				t.Errorf("%s: %v", o, err)
			}
		})
	}
	lookalike := "https://hugg" + string(rune(0xed)) + "ngface.co" // an accented i
	for name, o := range map[string]string{
		"a longer host that starts the same":     "https://huggingface.co.evil.example",
		"hf.co with more after it":               "https://hf.co.evil.example",
		"a host that ends the same":              "https://evilhuggingface.co",
		"hf.co with more before it":              "https://evilhf.co",
		"a user that looks like the host":        "https://huggingface.co@evil.example",
		"a user and password":                    "https://user:pass@huggingface.co",
		"a port":                                 "https://huggingface.co:8443",
		"the default port written out":           "https://hf.co:443",
		"upper case":                             "https://HuggingFace.co",
		"an upper-case subdomain":                "https://CDN-LFS.huggingface.co",
		"a lookalike letter":                     lookalike,
		"that lookalike in punycode":             "https://xn--huggngface-n8a.co",
		"plain http":                             "http://huggingface.co",
		"a path":                                 "https://huggingface.co/api",
		"a query":                                "https://huggingface.co?x=1",
		"a trailing dot":                         "https://huggingface.co.",
		"an empty label":                         "https://cdn..hf.co",
		"hf.space, where anyone can run a Space": "https://hf.space",
		"a Space":                                "https://someone.hf.space",
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			if err := check(o); err == nil || err.Error() != hfRefusal(o) {
				t.Errorf("%s: %v", o, err)
			}
		})
	}
}

// HF_ENDPOINT comes from the environment, read when the profiles are loaded. A profile file can't name it for
// itself, whatever fields it carries.
func TestHFEndpointIsReadAtLoadAndNeverFromTheProfile(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, root, `{"hfEndpoint":"https://mirror.example","profiles":[{"id":"huggingface","origins":["https://mirror.example"],
		"hfEndpoint":"https://mirror.example","HF_ENDPOINT":"https://mirror.example","endpoint":"https://mirror.example"}]}`)
	quiet := slog.New(&logRecords{})
	t.Setenv("HF_ENDPOINT", "")
	if got := hfTokenOrigins(root, quiet); len(got) != 0 {
		t.Errorf("the file named its own endpoint: %v", got)
	}
	t.Setenv("HF_ENDPOINT", "https://mirror.example/")
	loaded := loadOrFail(t, root)
	if got := loaded.Get(HFProfileID); got == nil || !slices.Equal(got.Origins, []string{"https://mirror.example"}) {
		t.Errorf("with HF_ENDPOINT naming it: %+v", got)
	}
	// What was loaded keeps what HF_ENDPOINT said when it was loaded.
	t.Setenv("HF_ENDPOINT", "")
	if got := loaded.Get(HFProfileID); got == nil || len(got.Origins) != 1 {
		t.Errorf("after HF_ENDPOINT changed: %+v", got)
	}
}

// A file written before this, or by hand, that names other origins still loads: they're left out, the rest of the
// profile works, and each one is said, in the log as well.
func TestLoadingLeavesOutOriginsTheHuggingFaceProfileCantName(t *testing.T) {
	t.Setenv("HF_ENDPOINT", "")
	root := t.TempDir()
	writeProfileFile(t, root, `{"profiles":[{"id":"huggingface","origins":["https://hf.co","https://evil.example"]}]}`)
	ps := loadOrFail(t, root)
	if got := ps.Get(HFProfileID); got == nil || !slices.Equal(got.Origins, []string{"https://hf.co"}) {
		t.Errorf("loaded: %+v", got)
	}
	want := `The huggingface download profile names "https://evil.example", which isn't Hugging Face's own, so the Hugging Face token isn't sent there.`
	if !slices.Equal(ps.Warnings, []string{want}) {
		t.Errorf("warnings: %q", ps.Warnings)
	}
	logged := &logRecords{}
	if got := hfTokenOrigins(root, slog.New(logged)); !slices.Equal(got, []string{"https://hf.co"}) {
		t.Errorf("token origins: %v", got)
	}
	if len(logged.got) != 1 || logged.got[0].Message != want || logged.got[0].Level != slog.LevelWarn {
		t.Fatalf("logged: %+v", logged.got)
	}
	event := ""
	logged.got[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "event" {
			event = a.Value.String()
		}
		return true
	})
	if event != "acquire.profile-origin-ignored" {
		t.Errorf("event %q", event)
	}
	// When it names nothing it can, it adds nothing, and isn't a profile that fails its checks.
	writeProfileFile(t, root, `{"profiles":[{"id":"huggingface","origins":["https://evil.example"]}]}`)
	ps = loadOrFail(t, root)
	if ps.Get(HFProfileID) != nil || ps.FirstInvalid() != nil || len(ps.Warnings) != 1 {
		t.Errorf("naming nothing it can: %+v, %v, %q", ps.Profiles, ps.FirstInvalid(), ps.Warnings)
	}
}

// What a file says is shown quoted, with anything that isn't printable escaped, and kept short.
func TestWhatAProfileFileSaysIsShownEscapedAndShort(t *testing.T) {
	t.Setenv("HF_ENDPOINT", "")
	root := t.TempDir()
	id := "v" + strings.Repeat("x", 5000) + "\x1b[2J"
	body, err := json.Marshal(map[string]any{"profiles": []any{
		map[string]any{"id": "huggingface", "origins": []string{"https://evil.example\x1b[2J", "https://" + strings.Repeat("a", 5000) + ".example"}},
		map[string]any{"id": id, "origins": []string{"https://a.example"}, "headers": map[string]string{"X-Key": "v"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, root, string(body))
	ps := loadOrFail(t, root)
	said := append([]string{}, ps.Warnings...)
	for _, err := range ps.Problems() {
		said = append(said, err.Error())
	}
	if len(said) != 3 || ps.Invalid(id) == nil {
		t.Fatalf("said: %q", said)
	}
	if !strings.Contains(said[0], `"https://evil.example\x1b[2J"`) {
		t.Errorf("not quoted: %q", said[0])
	}
	for _, s := range said {
		if strings.ContainsRune(s, 0x1b) || len(s) > 600 {
			t.Errorf("not escaped, or not short (%d bytes): %.150q", len(s), s)
		}
	}
}

// A profile that fails its checks is kept out of use, and naming it says why without its values.
func TestAProfileThatFailsItsChecksIsKeptOutOfUse(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, root, `{"profiles":[{"id":"vendor","origins":["https://vendor.example.com"],"headers":{"Host":"PLANTED-VALUE"}},
		{"id":"ok","origins":["https://ok.example.com"],"headers":{"X-Key":"v"}}]}`)
	ps := loadOrFail(t, root)
	if ps.Get("vendor") != nil || ps.Get("ok") == nil {
		t.Errorf("in use: %+v", ps.Profiles)
	}
	want := `download profile "vendor" is not valid: header Host is controlled by the downloader`
	if err := ps.Invalid("vendor"); err == nil || err.Error() != want {
		t.Errorf("invalid: %v", err)
	}
	if ps.Invalid("ok") != nil || ps.Invalid("missing") != nil || ps.FirstInvalid() == nil {
		t.Error("what's in use, or missing, isn't invalid")
	}
	_, err := Resolve(context.Background(), root, Request{Source: "https://vendor.example.com/m.gguf", AuthProfile: "vendor"})
	if err == nil || err.Error() != want {
		t.Errorf("a download naming it: %v", err)
	}
	if strings.Contains(fmt.Sprint(err, ps.Problems()), "PLANTED-VALUE") {
		t.Error("a message holds the header's value")
	}
}

// Adding and removing work on the file as written, so a profile that fails its checks can still be replaced or
// taken out, and isn't dropped by a change to another one.
func TestAddAndRemoveWorkOnTheFileAsWritten(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, root, `{"profiles":[{"id":"bad","origins":["https://a.example"],"headers":{"Host":"x"}}]}`)
	if err := SaveProfile(root, Profile{ID: "good", Origins: []string{"https://b.example"}, Headers: map[string]string{"X-Key": "v"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := readProfiles(root)
	if err != nil || len(raw.Profiles) != 2 {
		t.Fatalf("after adding: %+v %v", raw, err)
	}
	if removed, err := RemoveProfile(root, "bad"); err != nil || !removed {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if raw, err = readProfiles(root); err != nil || len(raw.Profiles) != 1 || raw.Profiles[0].ID != "good" {
		t.Errorf("after removing: %+v %v", raw, err)
	}
}

// The downloader's own headers are never a profile's, whatever the profile says: here one that skipped its
// checks, the way a file could.
func TestAReservedHeaderNeverComesFromAProfile(t *testing.T) {
	var sent http.Header
	p := &Profile{ID: "planted", Origins: []string{"https://vendor.example"}, Headers: map[string]string{"Range": "bytes=5-", "if-range": "planted", "X-Key": "v"}}
	tr := &scopedTransport{profile: p, base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = r.Header.Clone()
		return noContent(r), nil
	})}
	req, err := http.NewRequest(http.MethodGet, "https://vendor.example/f", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sent.Get("Range") != "bytes=0-" || sent.Get("If-Range") != "" || sent.Get("X-Key") != "v" {
		t.Errorf("sent: Range %q, If-Range %q, X-Key %q", sent.Get("Range"), sent.Get("If-Range"), sent.Get("X-Key"))
	}
}

// The token never goes where the huggingface profile can't send it, though the file names it. The same client,
// from the same file, sends it to the Hugging Face host the profile names, and to HF_ENDPOINT's origin.
func TestTheTokenNeverGoesWhereTheProfileCantSendIt(t *testing.T) {
	t.Setenv("HF_ENDPOINT", "")
	root := t.TempDir()
	writeProfileFile(t, root, `{"profiles":[{"id":"huggingface","origins":["https://evil.example","https://huggingface.co.evil.example","https://cas-bridge.xethub.hf.co"]}]}`)
	client := newClient(nil, "hf_planted_token", hfTokenOrigins(root, slog.New(&logRecords{})), false)
	sent := map[string]string{}
	client.Transport.(*scopedTransport).base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent[r.URL.Host] = r.Header.Get("Authorization")
		return noContent(r), nil
	})
	for _, host := range []string{"evil.example", "huggingface.co.evil.example", "cas-bridge.xethub.hf.co", "huggingface.co"} {
		resp, err := client.Get("https://" + host + "/f")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	want := map[string]string{"evil.example": "", "huggingface.co.evil.example": "", "cas-bridge.xethub.hf.co": "Bearer hf_planted_token", "huggingface.co": "Bearer hf_planted_token"}
	if !maps.Equal(sent, want) {
		t.Errorf("sent %q\nwant %q", sent, want)
	}
}
