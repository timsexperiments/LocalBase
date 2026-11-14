package system

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/timsexperiments/LocalBase/go/internal/catalog"
	"github.com/timsexperiments/LocalBase/go/internal/root"
	"github.com/timsexperiments/LocalBase/go/internal/store"
	"github.com/timsexperiments/LocalBase/go/test/harness/fsguard"
)

// A fresh install selects default models before any is downloaded, and selected isn't usable.
// Through the real binary, starting from the defaults `init` really chooses: every tool's page
// offers only what is on disk, and a tool with nothing to run shows no prompt that could only
// fail (DESIGN §27). Go only: TS has no Studio. No model runs here, so the files are sparse.
func TestStudioOffersOnlyModelsOnDisk(t *testing.T) {
	tools := []struct {
		page, column, onDisk, notOnDisk, none, filter string
	}{
		{"image", "selected_image_models", "dreamshaper-8-lcm", "juggernaut-xl-v9", "No picture model yet", "images"},
		{"video", "selected_video_models", "fastwan2.2-ti2v-5b-q6_k", "wan2.1-t2v-1.3b-q8_0", "No video model yet", "video"},
		{"speech", "selected_tts_models", "qwen3-tts-1.7b-base-q4_k_m", "kokoro-82m-v1.0-q8", "No speech model yet", "speech"},
		{"transcribe", "selected_stt_models", "whisper-base-q8_0", "whisper-small-q8_0", "No transcription model yet", "transcription"},
		{"decisions", "selected_llm_models", "clef-flash-q4_k_m", "clef-q4_k_m", "No decision model yet", "decisions"},
	}
	tmp := fsguard.ScratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true}
	if r := e.cli(nil, "--non-interactive", "init"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	serve := func() { e.serve("--no-auth", "--no-llm", "--no-stt", "--no-tts", "--no-image", "--no-video") }
	page := func(path string) string {
		t.Helper()
		r := e.do("GET", path, "")
		if r.Status != 200 {
			t.Fatalf("%s: %d", path, r.Status)
		}
		return string(r.Body)
	}
	hasPrompt := func(html string) bool {
		return strings.Contains(html, "data-docked") || strings.Contains(html, `id="decide"`)
	}

	// A fresh install: nothing can run, so no tool has a prompt, and each says how to get a model.
	serve()
	for _, tool := range tools {
		html := page("/app/studio/" + tool.page)
		if hasPrompt(html) || !strings.Contains(html, tool.none) || !strings.Contains(html, "/app/models/get?filter="+tool.filter) {
			t.Errorf("%s on a fresh install: a prompt, or no way to get a model", tool.page)
		}
	}
	fresh := page("/app/studio")
	if !strings.Contains(fresh, "Studio needs a model") {
		t.Error("the landing on a fresh install should say a model is needed")
	}
	for _, tool := range tools {
		if strings.Contains(fresh, `href="/app/studio/`+tool.page+`"`) {
			t.Errorf("the landing on a fresh install links %s as if it could run", tool.page)
		}
	}
	e.stop()

	// One of each tool's two selected models is on disk: only that one is offered.
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	r, err := root.Open(e.root)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	install := func(id string) *catalog.Spec {
		t.Helper()
		s := cat.ByID(id)
		if s == nil {
			t.Fatalf("no model %s", id)
		}
		dir := filepath.Join(e.root, "models", s.Kind)
		fsguard.MkdirAll(dir, 0o700)
		for _, a := range s.Artifacts {
			if err := fsguard.Sparse(filepath.Join(dir, a.Filename), a.ExpectedSizeBytes); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	specs := map[string]*catalog.Spec{}
	for _, tool := range tools {
		specs[tool.page] = install(tool.onDisk)
		// Added to what init selected: an active model must stay in its list.
		var saved string
		if err := st.DB.QueryRow(`SELECT ` + tool.column + ` FROM config`).Scan(&saved); err != nil {
			t.Fatal(err)
		}
		var ids []string
		if err := json.Unmarshal([]byte(saved), &ids); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{tool.onDisk, tool.notOnDisk} {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		list, _ := json.Marshal(ids)
		if _, err := st.DB.Exec(`UPDATE config SET `+tool.column+` = ?`, string(list)); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	serve()
	defer func() { e.stop() }()
	for _, tool := range tools {
		html := page("/app/studio/" + tool.page)
		if !hasPrompt(html) {
			t.Errorf("%s has a model on disk and no prompt", tool.page)
		}
		if !strings.Contains(html, `value="`+tool.onDisk+`"`) {
			t.Errorf("%s doesn't offer %s, which is on disk", tool.page, tool.onDisk)
		}
		if strings.Contains(html, `value="`+tool.notOnDisk+`"`) {
			t.Errorf("%s offers %s, which isn't on disk", tool.page, tool.notOnDisk)
		}
	}
	landing := page("/app/studio")
	if strings.Contains(landing, "Studio needs a model") {
		t.Error("the banner is for when nothing can run")
	}
	for _, tool := range tools {
		if !strings.Contains(landing, `<a href="/app/studio/`+tool.page+`" class="studio-card">`) {
			t.Errorf("the landing doesn't link %s, which can run", tool.page)
		}
	}

	// Half a model isn't a model: a video's encoder gone, a picture model's file the wrong size.
	video := specs["video"]
	if len(video.Artifacts) < 2 {
		t.Fatalf("%s should need more than one file", video.ModelID)
	}
	if err := fsguard.RemoveAll(filepath.Join(e.root, "models", video.Kind, video.Artifacts[len(video.Artifacts)-1].Filename)); err != nil {
		t.Fatal(err)
	}
	image := specs["image"]
	short := filepath.Join(e.root, "models", image.Kind, image.Artifacts[0].Filename)
	if err := fsguard.RemoveAll(short); err != nil {
		t.Fatal(err)
	}
	if err := fsguard.Sparse(short, image.Artifacts[0].ExpectedSizeBytes-1); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools[:2] {
		html := page("/app/studio/" + tool.page)
		if hasPrompt(html) || !strings.Contains(html, tool.none) {
			t.Errorf("%s offers a model that is only partly there", tool.page)
		}
	}
	if html := page("/app/studio/speech"); !hasPrompt(html) {
		t.Error("another tool's missing files shouldn't change this one")
	}
}

// Through the real binary: only a model that really can edit is marked as able to, and with none on
// disk the page says what is needed (DESIGN §27.5). No model runs here, so the files are sparse.
func TestImageEditOnlyWithAModelThatCan(t *testing.T) {
	const plain, capable = "dreamshaper-8-lcm", "juggernaut-xl-v9"
	tmp := fsguard.ScratchDir(t)
	e := &e2e{t: t, root: filepath.Join(tmp, "root"), outDir: tmp, noAuth: true}
	if r := e.cli(nil, "--non-interactive", "init"); r.code != 0 {
		t.Fatalf("init: %s", r.stderr)
	}
	serve := func() { e.serve("--no-auth", "--no-llm", "--no-stt", "--no-tts", "--no-image", "--no-video") }
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	install := func(id string) {
		t.Helper()
		s := cat.ByID(id)
		dir := filepath.Join(e.root, "models", s.Kind)
		fsguard.MkdirAll(dir, 0o700)
		for _, a := range s.Artifacts {
			if err := fsguard.Sparse(filepath.Join(dir, a.Filename), a.ExpectedSizeBytes); err != nil {
				t.Fatal(err)
			}
		}
	}
	r, err := root.Open(e.root)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(r.DB())
	if err != nil {
		t.Fatal(err)
	}
	list, _ := json.Marshal([]string{plain, capable})
	if _, err := st.DB.Exec(`UPDATE config SET selected_image_models = ?`, string(list)); err != nil {
		t.Fatal(err)
	}
	st.Close()
	page := func() string {
		t.Helper()
		res := e.do("GET", "/app/studio/image", "")
		if res.Status != 200 {
			t.Fatalf("image page: %d", res.Status)
		}
		return string(res.Body)
	}

	install(plain)
	serve()
	html := page()
	if !strings.Contains(html, `id="image-needs-edit"`) || !strings.Contains(html, `href="/app/models/get?filter=images"`) || strings.Contains(html, "data-edits") {
		t.Error("with no model that can edit, the page should say one is needed")
	}
	e.stop()

	install(capable)
	serve()
	defer func() { e.stop() }()
	html = page()
	if strings.Contains(html, `id="image-needs-edit"`) {
		t.Error("a model on disk can edit, so nothing should say one is needed")
	}
	if !strings.Contains(html, `value="`+capable+`" data-label="Juggernaut XL v9"  data-edits`) {
		t.Errorf("%s should be marked as able to edit", capable)
	}
	if strings.Contains(html, `value="`+plain+`" data-label="DreamShaper 8 LCM"  data-edits`) {
		t.Errorf("%s can't edit", plain)
	}
}
