package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/timsexperiments/LocalBase/go/internal/acquire"
	"github.com/timsexperiments/LocalBase/go/internal/auth"
	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/library"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
)

// The job page's cancel button is a plain form: it stops the import and shows the same
// page again, which keeps polling until the job has stopped.
func TestImportCancelRoute(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	r, err := root.Init(filepath.Join(dir, "root"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cat, err := catalog.LoadRoot(r.Dir, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	lib, err := library.Open(r, st, cat, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lib.StopImports(5 * time.Second) })
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		chunk := make([]byte, 16<<10)
		// Bounded, so a cancel that does nothing fails the test instead of filling the disk. It
		// then holds the download open, so it never finishes before the cancel lands.
		for sent := 0; sent < 64<<20; sent += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			if req.Header.Get("Range") == "bytes=0-0" {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		select {
		case <-req.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	t.Cleanup(src.Close)
	plan, err := acquire.Resolve(context.Background(), r.Dir, acquire.Request{Source: src.URL + "/stopme.gguf", AllowHTTP: true, AllowPrivate: true, Confirmed: true, ID: "stopme", Task: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := lib.StartImport(plan, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	as := func(perms ...string) *UI {
		return New(Deps{Library: lib, Catalog: cat, Store: st, RootDir: r.Dir, SignInEnabled: true,
			Principal: func(*http.Request) *auth.Principal {
				return &auth.Principal{Kind: "browser-session", ID: "u", Permissions: perms}
			}})
	}
	manager := as("models:manage", "models:read")
	form := func(id string) url.Values { return url.Values{"job": {id}} }
	// Reading models isn't enough, and neither is a post from elsewhere.
	if w := adminPost(as("models:read"), "/app/models/import/cancel", form(job)); w.Code != http.StatusForbidden {
		t.Errorf("without models:manage: %d", w.Code)
	}
	req := httptest.NewRequest("POST", "/app/models/import/cancel", strings.NewReader(form(job).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	manager.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("without the page's token: %d", w.Code)
	}
	if lib.Job(job).State != "running" {
		t.Fatalf("a refused request stopped the import: %s", lib.Job(job).State)
	}
	if w := adminPost(manager, "/app/models/import/cancel", form("imp_nope")); w.Code != http.StatusNotFound {
		t.Errorf("an unknown job: %d", w.Code)
	}
	// While it runs, its page offers a one-tap cancel for this job.
	if b := get(manager, "/app/models/import/jobs/"+job).Body.String(); !strings.Contains(b, ">Cancel import</button>") || !strings.Contains(b, `name="job" value="`+job+`"`) || !strings.Contains(b, `hx-post="/app/models/import/cancel"`) {
		t.Errorf("a running job offers no cancel: %.300s", b)
	}
	// The real one: the same job fragment comes back.
	w = adminPost(manager, "/app/models/import/cancel", form(job))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="import-job"`) {
		t.Fatalf("%d: %.200s", w.Code, w.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for lib.Job(job).State == "running" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if j := lib.Job(job); j.State != "cancelled" || j.ErrorCode != acquire.CodeCancelled {
		t.Errorf("after the cancel: %s %s", j.State, j.ErrorCode)
	}
	body := get(manager, "/app/models/import/jobs/"+job).Body.String()
	if !strings.Contains(body, "was cancelled") {
		t.Errorf("the job page: %.200s", body)
	}
	if strings.Contains(body, "Cancel import") {
		t.Errorf("a cancelled job still offers a cancel: %.300s", body)
	}
	// The job's own card says it calmly: it isn't shown as a failure.
	at := strings.Index(body, `id="import-job"`)
	if at < 0 {
		t.Fatalf("no job card: %.200s", body)
	}
	card := body[at:]
	if end := strings.Index(card, "</div>"); end >= 0 {
		card = card[:end]
	}
	if strings.Contains(card, "The import failed") || strings.Contains(card, `role="alert"`) {
		t.Errorf("a cancel shown as a failure: %s", card)
	}
}
