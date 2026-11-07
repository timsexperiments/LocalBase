package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What the huggingface profile names and can't is said in the gateway's own log, as an event, when an import is
// resolved. The source is a path on this computer, refused here without contacting anything.
func TestAnImportLogsWhatTheHuggingFaceProfileCantName(t *testing.T) {
	t.Setenv("HF_ENDPOINT", "")
	logs := &SyncBuffer{}
	ts, key, _, dir := importAPIGateway(t, slog.New(slog.NewJSONHandler(logs, nil)))
	profiles := filepath.Join(dir, "root", "credentials", "auth-profiles.json")
	if err := os.MkdirAll(filepath.Dir(profiles), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profiles, []byte(`{"profiles":[{"id":"huggingface","origins":["https://evil.example"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(map[string]any{"action": "resolve", "source": "./model.gguf", "task": "chat", "id": "x"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/_localbase/model-imports", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	want := `The huggingface download profile names "https://evil.example", which isn't Hugging Face's own, so the Hugging Face token isn't sent there.`
	for _, line := range strings.Split(logs.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["event"] == "acquire.profile-origin-ignored" && rec["msg"] == want && rec["level"] == "WARN" {
			return
		}
	}
	t.Errorf("not in the gateway's log (status %d):\n%s", resp.StatusCode, logs.String())
}
