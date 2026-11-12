package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The import page offers what a page can import: a repository or a web address, not a file
// on this computer. A plan whose size the source didn't say tells the person when it's known.
func TestImportPageCopy(t *testing.T) {
	p := newBackendPages(t)
	manager := p.ui("models:manage", "models:read")
	page := get(manager, "/app/models/import").Body.String()
	for _, want := range []string{"From a Hugging Face repository or a direct link.", `placeholder="org/repo or https://…"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the import page lacks %q", want)
		}
	}
	for _, gone := range []string{"a file on the LocalBase machine", "/path/on/the/server"} {
		if strings.Contains(page, gone) {
			t.Errorf("the import page still offers %q", gone)
		}
	}
	if err := p.st.SetSetting(context.Background(), store.PrivateImportsKey, "on"); err != nil {
		t.Fatal(err)
	}
	// No Content-Length and no Content-Range: the source doesn't say how big it is.
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		_, _ = w.Write(make([]byte, 1024))
	}))
	t.Cleanup(src.Close)
	w := adminPost(manager, "/app/models/import/resolve", url.Values{"source": {src.URL + "/m.gguf"}, "allow_http": {"1"}, "id": {"unsized"}, "task": {"chat"}})
	body := w.Body.String()
	if !strings.Contains(body, "Size checked when you confirm") || strings.Contains(body, "size not reported") {
		t.Errorf("an unsized plan: %d %.400s", w.Code, body)
	}
}
