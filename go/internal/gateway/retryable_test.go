package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/library"
)

// retryable checks the answer of a service that can't be reached for now: 503, Retry-After 5,
// service_unavailable, and the service's existing text.
func retryable(t *testing.T, what string, r result, service string) {
	t.Helper()
	var e struct {
		Error struct{ Type, Code, Message string }
	}
	json.Unmarshal([]byte(r.body), &e)
	want := service + " service is currently restarting or unavailable. Please try again shortly."
	if r.status != 503 || r.header.Get("Retry-After") != "5" || e.Error.Code != "service_unavailable" || e.Error.Message != want {
		t.Errorf("%s: %d, Retry-After %q, %s", what, r.status, r.header.Get("Retry-After"), r.body)
	}
}

// Review 02, M4: a key that can't be read is not a key that doesn't exist.
func TestAKeyThatCantBeReadIsRetryable(t *testing.T) {
	ts := newTestServer(t)
	s := runServer(ts)
	missing := post(t, ts, "/_localbase/api-keys", `{"action":"revoke","keyId":"key_missing"}`)
	if missing.status != 404 || missing.code() != "key_not_found" {
		t.Errorf("a missing key: %d %s", missing.status, missing.body)
	}
	s.Store.DB.Close()
	for _, body := range []string{`{"action":"revoke","keyId":"key_missing"}`, `{"action":"rotate","keyId":"key_missing"}`,
		`{"action":"set-scopes","keyId":"key_missing","scopes":["models:read"]}`} {
		retryable(t, body, post(t, ts, "/_localbase/api-keys", body), "Key management")
	}
}

// importGateway is the test gateway with a model library, and one model imported for embeddings.
func importGateway(t *testing.T) (*httptest.Server, *Server, result) {
	t.Helper()
	ts := newTestServer(t)
	s := runServer(ts)
	lib, err := library.Open(s.Root, s.Store, s.Catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Library = lib
	if err := s.Store.SaveImportedModel(context.Background(), "imported-embed", []byte(`{"id":"imported-embed","name":"Imported","tasks":["embeddings"]}`)); err != nil {
		t.Fatal(err)
	}
	return ts, s, post(t, ts, "/v1/chat/completions", `{"model":"imported-embed","messages":[{"role":"user","content":"hi"}]}`)
}

// Review 02, M5: an imported model is looked up by itself, and a lookup that fails is not a model
// that isn't imported.
func TestAnImportLookupThatFailsIsRetryable(t *testing.T) {
	ts, s, routed := importGateway(t)
	if routed.status != 400 || !strings.Contains(routed.body, "is imported for embeddings, not chat") {
		t.Errorf("an import is routed by its task: %d %s", routed.status, routed.body)
	}
	s.Store.DB.Close()
	for path, body := range map[string]string{
		"/v1/chat/completions": `{"model":"imported-x","messages":[{"role":"user","content":"hi"}]}`,
		"/v1/embeddings":       `{"model":"imported-x","input":"hi"}`,
		"/v1/systemone":        `{"model":"imported-x","state":"hi","questions":{"q":{"type":"choice","criteria":{"a":"x","b":"y"}}}}`,
	} {
		retryable(t, path, post(t, ts, path, body), "LLM")
	}
}

// The one-row lookup reads with the context it is given: a request's, which ends with it.
func TestTheImportLookupKeepsToItsContext(t *testing.T) {
	_, s, _ := importGateway(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e, err := s.Library.Imported(ctx, "imported-embed"); err == nil {
		t.Errorf("a cancelled context still read %v", e)
	}
	e, err := s.Library.Imported(context.Background(), "imported-embed")
	if err != nil || e == nil || !e.Imported || !e.Model.HasTask("embeddings") {
		t.Errorf("the import: %v %v", e, err)
	}
	if e, err := s.Library.Imported(context.Background(), "imported-x"); e != nil || err != nil {
		t.Errorf("a model that isn't imported: %v %v", e, err)
	}
}

// Review 02, M8: tts-server answers 503 while a cancelled generation finishes its window, and the
// gateway waits that out. It waits no longer than a speech request may take, and then the backend
// is unavailable for now.
func TestABusySpeechBackendIsWaitedOutForABoundedTime(t *testing.T) {
	ts := newSpeechGateway(t, &SyncBuffer{})
	s := runServer(ts)
	s.speechBusyFor = 500 * time.Millisecond
	client := &http.Client{Timeout: 30 * time.Second}
	start := time.Now()
	resp, err := client.Post(ts.URL+"/v1/audio/speech", "application/json", strings.NewReader(speechBody("fake:busy", "")))
	if err != nil {
		t.Fatalf("the backend was still being retried after %s: %v", time.Since(start).Round(time.Second), err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	r := result{resp.StatusCode, resp.Header, string(b)}
	retryable(t, "a backend busy for good", r, "TTS")
	if ok := post(t, ts, "/v1/audio/speech", speechBody("Hello there.", "")); ok.status != 200 {
		t.Errorf("speech that isn't busy: %d %s", ok.status, ok.body)
	}
}
